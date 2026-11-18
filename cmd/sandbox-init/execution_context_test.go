package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const executionContextUnavailable = 77

// This is an integration test of the real launch and re-exec paths. Every
// mount operation happens below an isolated outer mount namespace; the inner
// harness is PID 1 in its own PID namespace. The test parent owns its temporary
// cgroup, including cleanup if the outer helper times out. No production
// supervisor runs beside the harness: it is the sole child waiter.
func TestExecutionContextInheritance(t *testing.T) {
	unavailable := func(t *testing.T, reason string) {
		t.Helper()
		if os.Getenv("SANDBOX_INIT_REQUIRE_CONTEXT_TESTS") == "1" {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
	if os.Geteuid() != 0 {
		unavailable(t, "requires root with mount/PID/cgroup namespaces and a writable cgroup v2 hierarchy")
	}
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprintf("cgroup_control=%t", control), func(t *testing.T) {
			// Unlike t.TempDir, this has no private test-owned ancestor which
			// would prevent the non-root plugin from traversing to its workdir.
			root, err := os.MkdirTemp("", "sandbox-init-context-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			if err := os.Chmod(root, 0755); err != nil {
				t.Fatal(err)
			}
			cgroup, err := os.MkdirTemp(cgroupMountPoint, "sandbox-init-context-")
			if err != nil {
				unavailable(t, fmt.Sprintf("create isolated cgroup: %v", err))
			}
			// Register cleanup in this parent before starting either helper.
			// Killing the outer on timeout must not orphan the inner or lose
			// the only process that knows which cgroup belongs to this test.
			t.Cleanup(func() {
				if err := cleanupExecutionContextCgroup(cgroup, control); err != nil {
					t.Errorf("cleanup temporary cgroup %s: %v", cgroup, err)
				}
			})
			// Verify the bounded cleanup mechanism before any process can be
			// launched into the group. Do not require it after a timeout first.
			kill, err := os.OpenFile(filepath.Join(cgroup, "cgroup.kill"), os.O_WRONLY, 0)
			if err != nil {
				unavailable(t, fmt.Sprintf("temporary cgroup requires writable cgroup.kill: %v", err))
			}
			if err := kill.Close(); err != nil {
				t.Fatal(err)
			}
			if control {
				if err := os.Mkdir(filepath.Join(cgroup, "init"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/proc/self/exe", "test-execution-context-outer", root, strconv.FormatBool(control), cgroup)
			cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
			// An orphaned inner could retain CombinedOutput's pipe after the
			// outer is killed. Bound that drain so parent cleanup still runs.
			cmd.WaitDelay = 2 * time.Second
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOSYS) ||
				(errors.As(err, &exit) && exit.ExitCode() == executionContextUnavailable) {
				if os.Getenv("SANDBOX_INIT_REQUIRE_CONTEXT_TESTS") == "1" {
					t.Fatalf("required context integration test unavailable: %v\n%s", err, out)
				}
				t.Skipf("context integration test unavailable: %v\n%s", err, out)
			}
			if err != nil {
				t.Fatalf("context integration test: %v\n%s", err, out)
			}
		})
	}
}

// Only this exact group and its explicitly created init child are removed.
// cgroup.kill is recursive, so a timed-out inner cannot leave live descendants
// behind; cgroup.events confirms their exit before directories are removed.
func cleanupExecutionContextCgroup(cgroup string, control bool) error {
	populated := func() (bool, error) {
		data, err := os.ReadFile(filepath.Join(cgroup, "cgroup.events"))
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "populated" {
				switch fields[1] {
				case "0":
					return false, nil
				case "1":
					return true, nil
				}
			}
		}
		return false, fmt.Errorf("invalid cgroup.events: %q", data)
	}
	live, err := populated()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if live {
		if err := os.WriteFile(filepath.Join(cgroup, "cgroup.kill"), []byte("1"), 0); err != nil {
			return fmt.Errorf("kill remaining test processes: %w", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for live {
			live, err = populated()
			if err != nil {
				return err
			}
			if !live {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("test cgroup remained populated after cgroup.kill")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if control {
		if err := os.Remove(filepath.Join(cgroup, "init")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove init cgroup: %w", err)
		}
	}
	if err := os.Remove(cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove root cgroup: %w", err)
	}
	return nil
}

func runExecutionContextHelper() int {
	var err error
	switch os.Args[1] {
	case "test-execution-context-outer":
		err = executionContextOuter(os.Args[2], os.Args[3] == "true", os.Args[4])
	case "test-execution-context-inner":
		err = executionContextInner(os.Args[2], os.Args[3] == "true")
	case "test-execution-context-probe":
		err = executionContextProbe(os.Args[2], os.Args[3], os.Args[4], os.Args[5:]...)
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, err)
	var unavailable *executionContextPrerequisite
	if errors.As(err, &unavailable) {
		return executionContextUnavailable
	}
	return 1
}

type executionContextPrerequisite struct{ err error }

func (e *executionContextPrerequisite) Error() string { return e.err.Error() }

func executionContextOuter(root string, control bool, cgroup string) error {
	// First sever every inherited peer relationship. Setting this isolated
	// tree shared afterwards models the real outer -> inner propagation edge
	// without ever changing the test runner's mounts.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return &executionContextPrerequisite{fmt.Errorf("isolate outer mounts: %w", err)}
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("make isolated outer shared: %w", err)
	}
	cgroupFD, err := openCgroupDir(cgroup)
	if err != nil {
		return err
	}
	defer cgroupFD.Close()
	applicationCgroups.root, applicationCgroups.target = cgroupFD, cgroupFD
	attr := &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS}
	if err := cgroupConfigureClone(attr, true); err != nil {
		return err
	}
	mountpoint := filepath.Join(root, "mount")
	if err := os.Mkdir(mountpoint, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mountpoint, "outer-marker"), []byte("outer"), 0644); err != nil {
		return err
	}
	for _, shared := range []bool{true, false} {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("outer-late-shared-%t", shared)), 0755); err != nil {
			return err
		}
	}
	link, childLink, err := newExecutorPair()
	if err != nil {
		return err
	}
	defer link.close()
	defer childLink.Close()
	client := &executorClient{}
	go link.serve(func(msg executorMessage, fd *int) (executorMessage, error) {
		if msg.Op == "test-context-outer-mount-true" || msg.Op == "test-context-outer-mount-false" {
			shared := strings.TrimPrefix(msg.Op, "test-context-outer-mount-")
			target := filepath.Join(root, "outer-late-shared-"+shared)
			if err := unix.Mount("context-outer", target, "tmpfs", 0, "size=64k"); err != nil {
				return executorMessage{}, err
			}
			return executorMessage{}, os.WriteFile(filepath.Join(target, "outer-marker"), []byte("outer"), 0644)
		}
		if msg.Op != "test-context-ready" {
			return client.handleEvent(msg, fd)
		}
		if control {
			procs, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
			if err != nil {
				return executorMessage{}, err
			}
			if strings.TrimSpace(string(procs)) != "" {
				return executorMessage{}, fmt.Errorf("delegated root still contains processes: %s", procs)
			}
			return executorMessage{}, cgroupEnableAdvertisedControllers(cgroup)
		}
		return executorMessage{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe", "test-execution-context-inner", root, strconv.FormatBool(control))
	cmd.SysProcAttr = attr
	cmd.ExtraFiles = []*os.File{childLink}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return &executionContextPrerequisite{fmt.Errorf("clone inner into cgroup: %w", err)}
		}
		return fmt.Errorf("inner execution context: %w", err)
	}
	if marker, err := os.ReadFile(filepath.Join(mountpoint, "outer-marker")); err != nil || string(marker) != "outer" {
		return fmt.Errorf("plugin mount propagated back into outer: marker=%q err=%v", marker, err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "plugin-marker")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("plugin mount leaked into outer: %v", err)
	}
	for _, shared := range []bool{true, false} {
		marker := filepath.Join(root, fmt.Sprintf("plugin-late-shared-%t", shared), "plugin-marker")
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("late plugin mount leaked into outer: %s: %v", marker, err)
		}
	}
	return nil
}

type executionContextRecord struct {
	PID, UID, GID                     int
	PIDNamespace, MountNamespace      string
	CgroupNamespace, Cgroup, ProcSelf string
	Workdir, Token, MountMarker       string
	OuterMountMarker                  string
}

func readExecutionContext() (executionContextRecord, error) {
	r := executionContextRecord{PID: os.Getpid(), UID: os.Getuid(), GID: os.Getgid(), Token: os.Getenv("CONTEXT_TEST_TOKEN")}
	for path, dest := range map[string]*string{
		"/proc/self/ns/pid": &r.PIDNamespace, "/proc/self/ns/mnt": &r.MountNamespace,
		"/proc/self/ns/cgroup": &r.CgroupNamespace, "/proc/self": &r.ProcSelf,
	} {
		value, err := os.Readlink(path)
		if err != nil {
			return r, err
		}
		*dest = value
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return r, err
	}
	for _, line := range strings.Split(string(cgroup), "\n") {
		if strings.HasPrefix(line, "0::") {
			r.Cgroup = line
		}
	}
	r.Workdir, err = os.Getwd()
	return r, err
}

func executionContextProbe(report, mountpoint, operation string, outerMountpoint ...string) error {
	if err := checkExecutionContextDescriptors(); err != nil {
		return err
	}
	if operation == "mount" {
		if err := unix.Mount("context-test", mountpoint, "tmpfs", 0, "size=64k"); err != nil {
			return fmt.Errorf("plugin tmpfs mount: %w", err)
		}
		if err := os.WriteFile(filepath.Join(mountpoint, "plugin-marker"), []byte("plugin"), 0644); err != nil {
			return err
		}
	}
	r, err := readExecutionContext()
	if err != nil {
		return err
	}
	marker, err := os.ReadFile(filepath.Join(mountpoint, "plugin-marker"))
	if err != nil {
		return fmt.Errorf("read shared plugin mount: %w", err)
	}
	r.MountMarker = string(marker)
	if len(outerMountpoint) != 0 {
		marker, err := os.ReadFile(filepath.Join(outerMountpoint[0], "outer-marker"))
		if err != nil {
			return fmt.Errorf("read late outer mount: %w", err)
		}
		r.OuterMountMarker = string(marker)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(report+".tmp", encoded, 0600); err != nil {
		return err
	}
	if err := os.Rename(report+".tmp", report); err != nil {
		return err
	}
	if operation == "hold" {
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(report + ".release"); err == nil {
				return nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return errors.New("timed out waiting for primary release")
	}
	return nil
}

// Final user executables must not inherit launch synchronization, management
// sockets, namespace handles, or pinned cgroup directory capabilities. Check
// descriptor targets rather than fixed numbers: the Go runtime can legitimately
// reuse fd 3 for its own poll/event descriptors after the exec closes our fd.
func checkExecutionContextDescriptors() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return err
		}
		if fd < 3 {
			continue
		}
		path := filepath.Join("/proc/self/fd", entry.Name())
		target, err := os.Readlink(path)
		// ReadDir's own temporary descriptor is closed before we get here;
		// a runtime-owned descriptor may also disappear during this scan.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, prefix := range []string{"socket:[", "cgroup:[", "mnt:[", "pid:[", "net:[", "ipc:[", "uts:[", "user:[", "time:["} {
			if strings.HasPrefix(target, prefix) {
				return fmt.Errorf("internal descriptor leaked into final executable: fd=%d target=%s", fd, target)
			}
		}
		if target == cgroupMountPoint || strings.HasPrefix(target, cgroupMountPoint+"/") {
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("cgroup directory capability leaked into final executable: fd=%d target=%s", fd, target)
			}
		}
	}
	return nil
}

func executionContextInner(root string, control bool) error {
	if err := prepareInnerEnvironment(control, false); err != nil {
		return fmt.Errorf("prepare inner environment: %w", err)
	}
	file := os.NewFile(3, "context-test-control")
	unix.CloseOnExec(3)
	link, err := executorLinkFromFile(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	defer link.close()
	innerEvents = link
	go link.serve(func(executorMessage, *int) (executorMessage, error) {
		return executorMessage{}, errors.New("unexpected request to test inner")
	})
	if _, err := link.request(executorMessage{Op: "test-context-ready"}, -1); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return err
	}
	inner, err := readExecutionContext()
	if err != nil {
		return err
	}
	wantCgroup := "0::/"
	if control {
		wantCgroup += "init"
	}
	if inner.PID != 1 || inner.ProcSelf != "1" || inner.Cgroup != wantCgroup {
		return fmt.Errorf("inner was not scoped correctly: %+v", inner)
	}
	mountpoint := filepath.Join(root, "mount")
	for _, operation := range []string{"mount", "observe"} {
		report := filepath.Join(root, "plugin-"+operation+".json")
		spec := proto.PluginSpec{Exec: "/proc/self/exe", Args: []string{"test-execution-context-probe", report, mountpoint, operation}}
		pid, err := startPluginProcess(spec, nil, nil)
		if err != nil {
			return fmt.Errorf("start plugin %s: %w", operation, err)
		}
		if err := waitExecutionContextChild(pid); err != nil {
			return err
		}
		if err := checkExecutionContextReport(report, inner, false); err != nil {
			return fmt.Errorf("plugin %s: %w", operation, err)
		}
		// This check runs after the mounting plugin has exited. Its mount
		// must outlive that process and be visible to the inner and relaunch.
		if marker, err := os.ReadFile(filepath.Join(mountpoint, "plugin-marker")); err != nil || string(marker) != "plugin" {
			return fmt.Errorf("plugin mount disappeared after exit: %q, %v", marker, err)
		}
	}
	for _, shared := range []bool{true, false} {
		if err := checkExecutionContextPrimary(root, mountpoint, inner, shared); err != nil {
			return err
		}
	}
	return checkExecutionContextPluginUser(root, mountpoint, inner)
}

func checkExecutionContextPrimary(root, mountpoint string, inner executionContextRecord, shared bool) error {
	report := filepath.Join(root, fmt.Sprintf("primary-shared-%t.json", shared))
	lateMount := filepath.Join(root, fmt.Sprintf("plugin-late-shared-%t", shared))
	if err := os.Mkdir(lateMount, 0755); err != nil {
		return err
	}
	stderr, err := os.OpenFile("/dev/stderr", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	spec := &proto.LaunchSpec{SharePID: shared, Exec: "/proc/self/exe", Args: []string{"test-execution-context-probe", report, mountpoint, "hold"}}
	pid, outerPID, err := phase2ForkApp(spec, childStdio{stderr: stderr}, nil)
	if err != nil {
		return fmt.Errorf("start primary shared=%t: %w", shared, err)
	}
	if outerPID <= 0 {
		return fmt.Errorf("primary identity handshake returned %d", outerPID)
	}
	if err := checkExecutionContextReport(report, inner, !shared); err != nil {
		return fmt.Errorf("primary shared=%t: %w", shared, err)
	}
	primary, err := loadExecutionContextReport(report)
	if err != nil {
		return err
	}
	// Mount only after the primary has reported its namespace identity. A
	// private primary must receive this new event from the inner's shared
	// tree; seeing a mount copied at clone time would not prove propagation.
	pluginReport := report + ".late-plugin"
	pluginPID, err := startPluginProcess(proto.PluginSpec{
		Exec: "/proc/self/exe", Args: []string{"test-execution-context-probe", pluginReport, lateMount, "mount"},
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("start late mounting plugin shared=%t: %w", shared, err)
	}
	if err := waitExecutionContextChild(pluginPID); err != nil {
		return err
	}
	if err := checkExecutionContextReport(pluginReport, inner, false); err != nil {
		return fmt.Errorf("late plugin shared=%t: %w", shared, err)
	}
	// Also exercise the other direction of the intended propagation chain:
	// the outer creates a fresh mount after both inner and primary exist.
	outerMount := filepath.Join(root, fmt.Sprintf("outer-late-shared-%t", shared))
	if _, err := innerEvents.request(executorMessage{Op: fmt.Sprintf("test-context-outer-mount-%t", shared)}, -1); err != nil {
		return fmt.Errorf("create late outer mount: %w", err)
	}
	if marker, err := os.ReadFile(filepath.Join(outerMount, "outer-marker")); err != nil || string(marker) != "outer" {
		return fmt.Errorf("outer mount did not reach inner: %q, %v", marker, err)
	}
	// Exercise native exec while this exact primary generation is alive. The
	// registry's reaper is the only waiter here; no supervisor is started.
	reg := newExecRegistry()
	sup := &supervisorState{execReg: reg}
	sup.appPid.Store(int64(pid))
	execReport := report + ".exec"
	stderr, err = os.OpenFile("/dev/stderr", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	execPID, waiter, err := sup.forkExec(&proto.ExecSpec{Argv: []string{"/proc/self/exe", "test-execution-context-probe", execReport, lateMount, "observe", outerMount}}, childStdio{stderr: stderr})
	if err != nil {
		return fmt.Errorf("native exec shared=%t: %w", shared, err)
	}
	for {
		got, status, registered, err := reg.reapChild()
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if got == 0 || errors.Is(err, syscall.EINTR) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if got != execPID || !registered || !status.Exited() || status.ExitStatus() != 0 {
			return fmt.Errorf("unexpected exit during native exec: pid=%d want=%d registered=%t status=%v", got, execPID, registered, status)
		}
		if delivered := <-waiter; delivered != status {
			return fmt.Errorf("native exec status changed: %v -> %v", status, delivered)
		}
		break
	}
	if err := checkExecutionContextReport(execReport, primary, false); err != nil {
		return fmt.Errorf("native exec shared=%t: %w", shared, err)
	}
	executed, err := loadExecutionContextReport(execReport)
	if err != nil {
		return err
	}
	if executed.OuterMountMarker != "outer" {
		return fmt.Errorf("native exec did not observe outer -> inner -> primary mount: %+v", executed)
	}
	if err := os.WriteFile(report+".release", nil, 0600); err != nil {
		return err
	}
	return waitExecutionContextChild(pid)
}

func checkExecutionContextPluginUser(root, mountpoint string, inner executionContextRecord) error {
	private, public, workdir := filepath.Join(root, "private"), filepath.Join(root, "public"), filepath.Join(root, "user-work")
	for _, dir := range []string{private, public, workdir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			return err
		}
	}
	if err := os.Chmod(private, 0700); err != nil {
		return err
	}
	if err := os.Chown(workdir, 65534, 65534); err != nil {
		return err
	}
	// A procfs executable symlink is not a suitable non-root fixture: after
	// dropping credentials it can depend on access to the original Go build
	// location. Give the plugin an executable with a fully traversable path.
	probeImage, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		return err
	}
	publicProbe := filepath.Join(public, "context-probe")
	if err := os.WriteFile(publicProbe, probeImage, 0755); err != nil {
		return err
	}
	if err := os.Chmod(publicProbe, 0755); err != nil {
		return err
	}
	// Root can find this earlier PATH entry; the configured plugin user must
	// skip its unsearchable parent and execute the public probe instead.
	if err := os.WriteFile(filepath.Join(private, "context-probe"), []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
		return err
	}
	spec := proto.PluginSpec{
		Exec: "context-probe", Args: []string{"test-execution-context-probe", "user.json", mountpoint, "observe"},
		User: "65534:65534", Workdir: workdir,
		Env: map[string]string{"PATH": private + ":" + public, "CONTEXT_TEST_TOKEN": "plugin-user"},
	}
	pid, err := startPluginProcess(spec, nil, nil)
	if err != nil {
		return fmt.Errorf("start non-root plugin: %w", err)
	}
	if err := waitExecutionContextChild(pid); err != nil {
		return err
	}
	report := filepath.Join(workdir, "user.json")
	if err := checkExecutionContextReport(report, inner, false); err != nil {
		return err
	}
	r, err := loadExecutionContextReport(report)
	if err != nil {
		return err
	}
	if r.UID != 65534 || r.GID != 65534 || r.Workdir != workdir || r.Token != "plugin-user" {
		return fmt.Errorf("plugin identity/environment changed: %+v", r)
	}
	return nil
}

func waitExecutionContextChild(pid int) error {
	var status syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if !status.Exited() || status.ExitStatus() != 0 {
			return fmt.Errorf("context child %d exited with %v", pid, status)
		}
		return nil
	}
}

func loadExecutionContextReport(path string) (executionContextRecord, error) {
	var r executionContextRecord
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(data, &r)
			return r, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r, fmt.Errorf("timed out waiting for context report %s", path)
}

func checkExecutionContextReport(path string, reference executionContextRecord, private bool) error {
	r, err := loadExecutionContextReport(path)
	if err != nil {
		return err
	}
	if r.ProcSelf != strconv.Itoa(r.PID) || r.CgroupNamespace != reference.CgroupNamespace || r.Cgroup != reference.Cgroup || r.MountMarker != "plugin" {
		return fmt.Errorf("wrong proc/cgroup/mount view: got %+v, reference %+v", r, reference)
	}
	if private {
		if r.PID != 1 || r.PIDNamespace == reference.PIDNamespace || r.MountNamespace == reference.MountNamespace {
			return fmt.Errorf("private primary lost isolation: got %+v, inner %+v", r, reference)
		}
	} else if r.PID == 1 || r.PIDNamespace != reference.PIDNamespace || r.MountNamespace != reference.MountNamespace {
		return fmt.Errorf("child did not inherit execution context: got %+v, reference %+v", r, reference)
	}
	return nil
}
