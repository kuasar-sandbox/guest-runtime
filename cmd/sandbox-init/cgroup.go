// cgroup v2 wiring for the user execution environment and snapshot freezer.
//
// The real /sys/fs/cgroup/sandbox cgroup is both the user cgroup namespace
// root and the recursive snapshot freeze root. launch.cgroup_control
// selects the process layout below it:
//
//	false: inner sandbox-init and its children live in /sandbox and see 0::/
//	true:  /sandbox stays empty; inner sandbox-init and its default children
//	       live in /sandbox/init and see 0::/init
//
// The outer sandbox-init remains outside this tree. It atomically clones the
// inner init into /sandbox, where the fork child creates the cgroup namespace.
// The inner installs its scoped view and (true only) moves itself to /init
// before any user code runs. Later user processes are born in the final target.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	cgroupMountPoint     = "/sys/fs/cgroup"
	sandboxCgroupDir     = cgroupMountPoint + "/sandbox"
	sandboxInitCgroupDir = sandboxCgroupDir + "/init"
	cgroupFreezeFile     = sandboxCgroupDir + "/cgroup.freeze"
	cgroupEventsFile     = sandboxCgroupDir + "/cgroup.events"
	sandboxCgroupProcs   = sandboxCgroupDir + "/cgroup.procs"

	// This path is evaluated only inside the inner init's scoped cgroup
	// mount. It resolves to real /sandbox/init/cgroup.procs and is the only
	// cgroup.procs file this implementation writes.
	scopedInitCgroupProcs = cgroupMountPoint + "/init/cgroup.procs"

	// freezeConfirmTimeout bounds the wait for cgroup.events:frozen 1 after
	// writing cgroup.freeze=1. It stays inside proto.DeadlineQuiesce so sync,
	// transport teardown, and the response retain time to complete.
	freezeConfirmTimeout = 3 * time.Second
	freezePollInterval   = 5 * time.Millisecond
)

// applicationCgroups owns the long-lived capabilities sandbox-init needs for
// atomic placement and namespace joining. Every descriptor is O_CLOEXEC and is
// passed to re-exec helpers only through an explicit ExtraFiles slot.
var applicationCgroups struct {
	sync.RWMutex
	control       bool
	processGroups bool     // preserve slot workloads' independent process groups
	root          *os.File // /sandbox (outer) or its scoped / (inner)
	target        *os.File // root or init leaf, used for direct clone placement
	namespace     *os.File // the inner init's cgroup namespace
}

// cgroupMount mounts the guest-global cgroup v2 hierarchy, propagates root
// controllers, creates /sandbox and optional /sandbox/init, and pins root and
// final-target directory FDs. Called once from phase 1 after switch-root.
//
// cgroup_control=true is a delegation contract: every controller advertised at
// the guest root must be enabled and observed in subtree_control. false keeps
// the historical best-effort root propagation because no application subtree
// is promised in that mode.
func cgroupMount(control bool) error {
	if err := cgroupMountDelegated(); err != nil {
		return err
	}
	if err := cgroupEnableAdvertisedControllers(cgroupMountPoint); err != nil {
		if control {
			return fmt.Errorf("enable guest-root controllers: %w", err)
		}
		logf("cgroup: guest-root controller delegation unavailable (continuing without application delegation): %v", err)
	}
	if err := os.Mkdir(sandboxCgroupDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mkdir %s: %w", sandboxCgroupDir, err)
	}
	if control {
		if err := os.Mkdir(sandboxInitCgroupDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", sandboxInitCgroupDir, err)
		}
	}

	root, err := openCgroupDir(sandboxCgroupDir)
	if err != nil {
		return fmt.Errorf("open application cgroup root: %w", err)
	}
	targetPath := sandboxCgroupDir
	if control {
		targetPath = sandboxInitCgroupDir
	}
	target, err := openCgroupDir(targetPath)
	if err != nil {
		_ = root.Close()
		return fmt.Errorf("open application cgroup target: %w", err)
	}

	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.root != nil || applicationCgroups.target != nil || applicationCgroups.namespace != nil {
		_ = root.Close()
		_ = target.Close()
		return errors.New("application cgroups already initialized")
	}
	applicationCgroups.control = control
	applicationCgroups.root = root
	applicationCgroups.target = target
	return nil
}

// nsdelegate is a Guest-wide cgroup option. Only the outer init enables it;
// setting the option from an inner namespace would be silently ignored by the
// kernel. Verify the mounted superblock instead of accepting that fallback.
func cgroupMountDelegated() error {
	return cgroupMountDelegatedWith(unix.Mount, os.ReadFile)
}

func cgroupMountDelegatedWith(
	mount func(string, string, string, uintptr, string) error,
	read func(string) ([]byte, error),
) error {
	if err := mount("cgroup2", cgroupMountPoint, "cgroup2", 0, "nsdelegate"); err != nil {
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount delegated cgroup2 on %s: %w", cgroupMountPoint, err)
		}
		if err := mount("cgroup2", cgroupMountPoint, "cgroup2", unix.MS_REMOUNT, "nsdelegate"); err != nil {
			return fmt.Errorf("enable cgroup nsdelegate: %w", err)
		}
	}
	info, err := read("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("verify cgroup nsdelegate: %w", err)
	}
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[4] != cgroupMountPoint {
			continue
		}
		for i := 6; i+3 < len(fields); i++ {
			if fields[i] != "-" || fields[i+1] != "cgroup2" {
				continue
			}
			for _, option := range strings.Split(fields[i+3], ",") {
				if option == "nsdelegate" {
					return nil
				}
			}
		}
	}
	return errors.New("guest cgroup2 mount did not enable nsdelegate")
}

func openCgroupDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// cgroupConfigureClone adds atomic cgroup placement to an os/exec launch.
// bootstrap is used only by the outer init to create the inner init at the
// environment root and unshare its cgroup namespace in the fork child. All
// user launches originate in the inner init and use the pinned final target.
func cgroupConfigureClone(attr *syscall.SysProcAttr, bootstrap bool) error {
	if attr == nil {
		return errors.New("nil SysProcAttr")
	}
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	target := applicationCgroups.target
	if bootstrap {
		target = applicationCgroups.root
	}
	if target == nil {
		return errors.New("application cgroup target is not initialized")
	}
	if !bootstrap && applicationCgroups.namespace == nil {
		return errors.New("application cgroup namespace is not initialized")
	}
	// A process in the autofs daemon process group can bypass its triggers.
	// The inner is always in a separate group. Its non-TTY children retain
	// the old per-process grouping when slots are present; a re-exec cannot
	// rediscover this from the outer's process-local guestSlots registry.
	if (bootstrap || applicationCgroups.processGroups) && !attr.Setsid {
		attr.Setpgid = true
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = int(target.Fd())
	if bootstrap {
		// Go performs this unshare in the single-threaded fork child, after
		// clone3(CLONE_INTO_CGROUP) and before re-execing sandbox-init. That
		// ordering makes real /sandbox the namespace root.
		attr.Unshareflags |= unix.CLONE_NEWCGROUP
	}
	return nil
}

func managedProcessGroupsEnabled() bool {
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	return applicationCgroups.processGroups
}

// cgroupNamespaceFile returns sandbox-init's pinned application cgroup
// namespace capability for an explicit ExtraFiles handoff.
func cgroupNamespaceFile() (*os.File, error) {
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	if applicationCgroups.namespace == nil {
		return nil, errors.New("application cgroup namespace is not initialized")
	}
	return applicationCgroups.namespace, nil
}

// cgroupControlEnabled reports the immutable launch topology selected at boot.
func cgroupControlEnabled() bool {
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	return applicationCgroups.control
}

// cgroupFinalizeBootstrap pins the inner init's namespace while it is blocked
// in the internal handshake. In delegation mode it proves /sandbox has no
// direct processes and enables every advertised controller before user code.
func cgroupFinalizeBootstrap(pid int) error {
	path := fmt.Sprintf("/proc/%d/ns/cgroup", pid)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("pin %s: %w", path, err)
	}
	ns := os.NewFile(uintptr(fd), path)
	fail := func(err error) error {
		_ = ns.Close()
		return err
	}

	if cgroupControlEnabled() {
		procs, err := os.ReadFile(sandboxCgroupProcs)
		if err != nil {
			return fail(fmt.Errorf("verify application root processes: %w", err))
		}
		if strings.TrimSpace(string(procs)) != "" {
			return fail(fmt.Errorf("verify application root processes: %s is not empty", sandboxCgroupProcs))
		}
		if err := cgroupEnableAdvertisedControllers(sandboxCgroupDir); err != nil {
			return fail(fmt.Errorf("enable application-root controllers: %w", err))
		}
	}

	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.namespace != nil {
		return fail(errors.New("application cgroup namespace already pinned"))
	}
	applicationCgroups.namespace = ns
	return nil
}

// cgroupEnableAdvertisedControllers enables every name currently listed in
// dir/cgroup.controllers and verifies each one appears in the readback of
// dir/cgroup.subtree_control. A delegation request never silently degrades.
func cgroupEnableAdvertisedControllers(dir string) error {
	return cgroupEnableAdvertisedControllersWith(dir, os.ReadFile, os.WriteFile)
}

func cgroupEnableAdvertisedControllersWith(
	dir string,
	read func(string) ([]byte, error),
	write func(string, []byte, os.FileMode) error,
) error {
	controllersPath := dir + "/cgroup.controllers"
	subtreePath := dir + "/cgroup.subtree_control"
	available, err := read(controllersPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", controllersPath, err)
	}
	controllers := strings.Fields(string(available))
	if len(controllers) == 0 {
		return nil
	}
	enable := "+" + strings.Join(controllers, " +")
	if err := write(subtreePath, []byte(enable), 0); err != nil {
		return fmt.Errorf("write %s %q: %w", subtreePath, enable, err)
	}
	enabled, err := read(subtreePath)
	if err != nil {
		return fmt.Errorf("read back %s: %w", subtreePath, err)
	}
	if missing := missingControllers(controllers, strings.Fields(string(enabled))); len(missing) > 0 {
		return fmt.Errorf("verify %s: controllers not enabled: %s", subtreePath, strings.Join(missing, ","))
	}
	return nil
}

func missingControllers(want, got []string) []string {
	set := make(map[string]struct{}, len(got))
	for _, controller := range got {
		controller = strings.TrimLeft(controller, "+-")
		set[controller] = struct{}{}
	}
	var missing []string
	for _, controller := range want {
		if _, ok := set[controller]; !ok {
			missing = append(missing, controller)
		}
	}
	return missing
}

// cgroupJoinNamespace joins the inner init's pinned cgroup namespace. It is
// called only by short-lived, OS-thread-locked re-exec helpers.
func cgroupJoinNamespace(fd int) error {
	return cgroupJoinNamespaceWith(fd, unix.Setns)
}

func cgroupJoinNamespaceWith(fd int, setns func(int, int) error) error {
	if fd < 0 {
		return fmt.Errorf("invalid cgroup namespace fd %d", fd)
	}
	if err := setns(fd, unix.CLONE_NEWCGROUP); err != nil {
		return fmt.Errorf("setns cgroup: %w", err)
	}
	return nil
}

// cgroupMountScoped replaces the inherited guest-global cgroup2 mount in a
// child's private, rslave mount namespace. Because the calling task is already
// in the user cgroup namespace, the new mount exposes real /sandbox as /.
func cgroupMountScoped() error {
	return cgroupMountScopedWith(unix.Unmount, unix.Mount)
}

func cgroupMountScopedWith(
	unmount func(string, int) error,
	mount func(string, string, string, uintptr, string) error,
) error {
	if err := unmount(cgroupMountPoint, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount inherited cgroup2 from %s: %w", cgroupMountPoint, err)
	}
	if err := mount("cgroup2", cgroupMountPoint, "cgroup2", 0, ""); err != nil {
		return fmt.Errorf("mount scoped cgroup2 on %s: %w", cgroupMountPoint, err)
	}
	return nil
}

// cgroupMoveBootstrapToInit performs the one explicitly permitted
// cgroup.procs write: the inner init moves itself from virtual / to /init
// after its scoped cgroupfs is mounted. All later starts use
// CLONE_INTO_CGROUP on the final target and never migrate after birth.
func cgroupMoveBootstrapToInit() error {
	return cgroupMoveBootstrapToInitWith(os.WriteFile)
}

func cgroupMoveBootstrapToInitWith(write func(string, []byte, os.FileMode) error) error {
	if err := write(scopedInitCgroupProcs, []byte("0"), 0); err != nil {
		return fmt.Errorf("move bootstrap child to /init: %w", err)
	}
	return nil
}

// cgroupFreeze freezes the complete real /sandbox subtree and blocks until the
// kernel confirms cgroup.events:frozen 1, or freezeConfirmTimeout elapses.
func cgroupFreeze() error {
	if err := os.WriteFile(cgroupFreezeFile, []byte("1"), 0); err != nil {
		return fmt.Errorf("write cgroup.freeze=1: %w", err)
	}
	deadline := time.Now().Add(freezeConfirmTimeout)
	for {
		frozen, err := cgroupFrozen()
		if err != nil {
			return err
		}
		if frozen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup.events frozen 1 not observed within %s", freezeConfirmTimeout)
		}
		time.Sleep(freezePollInterval)
	}
}

// cgroupThaw unfreezes the complete real /sandbox subtree. Writing 0 is
// idempotent, so restore and attach can call it unconditionally.
func cgroupThaw() error {
	if err := os.WriteFile(cgroupFreezeFile, []byte("0"), 0); err != nil {
		return fmt.Errorf("write cgroup.freeze=0: %w", err)
	}
	return nil
}

// cgroupFrozen reports whether real /sandbox currently reads frozen 1.
func cgroupFrozen() (bool, error) {
	b, err := os.ReadFile(cgroupEventsFile)
	if err != nil {
		return false, fmt.Errorf("read cgroup.events: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "frozen" {
			return f[1] == "1", nil
		}
	}
	return false, nil
}
