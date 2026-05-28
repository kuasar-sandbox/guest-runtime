// cgroup v2 freezer wiring for sandbox-init.
//
// The platform guest kernel enables CONFIG_CGROUPS=y solely for the v2
// freezer (no resource controllers — see docs/sandbox-kernel.md §5.2).
// sandbox-init creates one fixed `app` cgroup, places the user-app
// process tree in it, and freezes it before a snapshot quiesce / thaws
// it once the restore environment is rebuilt. This makes the snapshot
// capture the app stopped and keeps it stopped across /vm.resume until
// sandbox-init has re-fixed the environment, eliminating the
// resume-vs-env-init race (docs/sandbox-runtime.md §3.4).
//
// All work is plain syscalls + cgroupfs writes; no external tools.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	cgroupMountPoint = "/sys/fs/cgroup"
	appCgroupDir     = cgroupMountPoint + "/app"
	cgroupFreezeFile = appCgroupDir + "/cgroup.freeze"
	cgroupEventsFile = appCgroupDir + "/cgroup.events"
	cgroupProcsFile  = appCgroupDir + "/cgroup.procs"

	// freezeConfirmTimeout bounds the wait for cgroup.events:frozen 1
	// after writing cgroup.freeze=1. The freezer stops a quiescing app
	// (post sync/drop_caches, tasks in S/R) within milliseconds; only a
	// task in long uninterruptible sleep can stall it. Kept well inside
	// proto.DeadlineQuiesce (8 s) so the remaining quiesce steps (sync,
	// drop_caches, MUX close) still fit the host deadline.
	freezeConfirmTimeout = 3 * time.Second
	freezePollInterval   = 5 * time.Millisecond
)

// cgroupMount mounts the cgroup v2 hierarchy and creates the single
// fixed `app` cgroup. Called from phase 1 after the chroot so the path
// is stable for the whole sandbox lifetime. No controllers are enabled
// in subtree_control — freezer is core, controllers stay off. The
// kernel provides the /sys/fs/cgroup mount point when CONFIG_CGROUPS=y;
// EBUSY means it is already mounted, which is fine.
//
// sandbox-init (PID 1) deliberately stays in the root cgroup so a freeze
// of `app` never stops the supervisor / vsock listener itself.
func cgroupMount() error {
	if err := unix.Mount("cgroup2", cgroupMountPoint, "cgroup2", 0, ""); err != nil {
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount cgroup2 on %s: %w", cgroupMountPoint, err)
		}
	}
	if err := os.Mkdir(appCgroupDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mkdir %s: %w", appCgroupDir, err)
	}
	return nil
}

// cgroupPlaceApp moves pid into the `app` cgroup. The app's descendants
// inherit the cgroup, so the whole tree lands in the freeze domain.
// This is a correctness precondition for the snapshot freeze (a pid left
// in the root cgroup would not be frozen, silently defeating the freeze
// and reintroducing the resume-vs-env race) — the caller treats failure
// as fatal.
func cgroupPlaceApp(pid int) error {
	if err := os.WriteFile(cgroupProcsFile, []byte(strconv.Itoa(pid)), 0); err != nil {
		return fmt.Errorf("write %s pid=%d: %w", cgroupProcsFile, pid, err)
	}
	return nil
}

// cgroupFreeze freezes the app cgroup and blocks until the kernel
// confirms cgroup.events:frozen 1, or freezeConfirmTimeout elapses.
// A confirmed freeze is a hard precondition for `quiesced`: a
// half-frozen snapshot is exactly the resume-vs-env race we eliminate,
// so on timeout this returns an error and the caller must NOT send
// quiesced (host then abandons the snapshot; see docs/sandbox-runtime.md
// §3.4 错误处理).
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

// cgroupThaw unfreezes the app cgroup. Idempotent: writing 0 to an
// already-thawed (or never-frozen) cgroup is a no-op, so both the
// restore path (after env rebuild) and the attach path (snapshot +
// resume_after, where the plain MUX-reconnect case was never frozen)
// call it unconditionally.
func cgroupThaw() error {
	if err := os.WriteFile(cgroupFreezeFile, []byte("0"), 0); err != nil {
		return fmt.Errorf("write cgroup.freeze=0: %w", err)
	}
	return nil
}

// cgroupFrozen reports whether cgroup.events currently reads frozen 1.
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
