package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const (
	systemdSource       = "/usr/lib/sandbox-runtime/systemd"
	systemdPrefix       = "/run/sandbox-runtime/systemd"
	systemdControl      = "/run/sandbox-init"
	systemdService      = "sandbox-init.service"
	systemdTarget       = "sandbox-init.target"
	systemdExecutorPath = "/opt/sandbox-runtime/bin/sandbox-init"
	// Both supported Linux/glibc ABIs reserve RT signals 32 and 33. This is
	// libc SIGRTMIN+4, not the kernel's unadjusted SIGRTMIN+4.
	systemdPoweroffSignal = syscall.Signal(38)
)

var systemdPayload *os.File

var runtimeSystemdTools bool // service-process local; helpers receive the resulting environment

func failSystemdLaunch(conn *vsockConn, err error) {
	initialLaunch.failed(err)
	logf("systemd Launch failed: %v", err)
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: err.Error()})
	doReboot()
}

func executionDefaultPath() string {
	const path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if runtimeSystemdTools {
		return systemdPrefix + "/bin:" + path
	}
	return path
}

func systemdEnabled(spec *proto.LaunchSpec) bool {
	return spec != nil && spec.Systemd != "" && spec.Systemd != "off"
}

func validateSystemdMode(mode string) error {
	switch mode {
	case "", "off", "auto", "runtime", "provided":
		return nil
	default:
		return fmt.Errorf("invalid launch.systemd %q", mode)
	}
}

// Pin the source while the Runtime root is still accessible. It is not a
// workload mount: off/provided never receive this tree, including through /opt.
func pinSystemdPayload(spec *proto.LaunchSpec) error {
	if err := validateSystemdMode(spec.Systemd); err != nil {
		return err
	}
	if spec.Systemd != "auto" && spec.Systemd != "runtime" {
		return nil
	}
	f, err := openCgroupDir(systemdSource) // O_DIRECTORY | O_CLOEXEC
	if spec.Systemd == "auto" && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Runtime systemd payload: %w", err)
	}
	systemdPayload = f
	return nil
}

type systemdInstallation struct {
	manager   string
	runtime   bool
	protected []string
}

func findProvidedSystemd() (string, error) {
	return findSystemdCandidates([]string{"/usr/lib/systemd/systemd", "/lib/systemd/systemd", "/sbin/init"})
}

func findSystemdCandidates(candidates []string) (string, error) {
	var selected string
	var identity os.FileInfo
	for _, candidate := range candidates {
		st, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("systemd candidate %s: %w", candidate, err)
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", fmt.Errorf("systemd candidate %s: %w", candidate, err)
		}
		// /sbin/init can legitimately be a different init. Only its resolved
		// manager name participates; explicit systemd paths never get ignored.
		if filepath.Base(candidate) == "init" && filepath.Base(resolved) != "systemd" {
			continue
		}
		st, err = os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if identity != nil {
			if !os.SameFile(identity, st) {
				return "", fmt.Errorf("conflicting systemd managers %s and %s", selected, resolved)
			}
			continue
		}
		if err := inspectSystemdManager(resolved); err != nil {
			return "", fmt.Errorf("systemd candidate %s: %w", candidate, err)
		}
		selected, identity = resolved, st
	}
	return selected, nil
}

func selectSystemd(spec *proto.LaunchSpec) (_ *systemdInstallation, err error) {
	if err := validateSystemdMode(spec.Systemd); err != nil {
		return nil, err
	}
	if systemdPayload != nil {
		defer func() { _ = systemdPayload.Close(); systemdPayload = nil }()
	}
	if !systemdEnabled(spec) {
		return nil, nil
	}
	installation := &systemdInstallation{}
	if spec.Systemd != "runtime" {
		installation.manager, err = findProvidedSystemd()
		if err != nil {
			return nil, err
		}
		if installation.manager == "" && spec.Systemd == "provided" {
			return nil, errors.New("launch.systemd=provided requires a rootfs systemd installation")
		}
	}
	if installation.manager == "" {
		if systemdPayload == nil {
			return nil, errors.New("Runtime systemd payload was not pinned")
		}
		if _, err := os.Lstat(systemdPrefix); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reserved Runtime systemd path %s is occupied or inaccessible: %v", systemdPrefix, err)
		}
		if err := mkdirReservedParent("/run/sandbox-runtime"); err != nil {
			return nil, err
		}
		if err := os.Mkdir(systemdPrefix, 0o755); err != nil {
			return nil, err
		}
		if err := unix.Mount(fmt.Sprintf("/proc/self/fd/%d", systemdPayload.Fd()), systemdPrefix, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return nil, fmt.Errorf("project Runtime systemd: %w", err)
		}
		if err := unix.Mount("", systemdPrefix, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return nil, fmt.Errorf("protect Runtime systemd: %w", err)
		}
		installation.manager, installation.runtime = systemdPrefix+"/lib/systemd/systemd", true
		if err := inspectSystemdManager(installation.manager); err != nil {
			return nil, fmt.Errorf("Runtime systemd: %w", err)
		}
	}
	logf("systemd selected policy=%s manager=%s runtime=%t", spec.Systemd, installation.manager, installation.runtime)
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	entry, err := os.Stat(systemdExecutorPath)
	if err != nil || !os.SameFile(self, entry) {
		return nil, fmt.Errorf("stable systemd executor %s was masked or replaced: %v", systemdExecutorPath, err)
	}
	return installation, nil
}

func mkdirReservedParent(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		st, statErr := os.Lstat(path)
		if statErr != nil || !st.IsDir() {
			return fmt.Errorf("reserved parent %s is not a directory", path)
		}
	}
	return nil
}

func systemdGrace(spec *proto.LaunchSpec) time.Duration {
	if spec != nil && spec.StopGraceSec > 0 {
		return time.Duration(spec.StopGraceSec) * time.Second
	}
	return gracefulShutdown
}

func systemdUnitName(path string) string {
	if path == "/" {
		return "-.mount"
	}
	var b strings.Builder
	for i, c := range []byte(strings.Trim(path, "/")) {
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == ':', c == '.' && i != 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String() + ".mount"
}

func writeReserved(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("reserve %s: %w", path, err)
	}
	_, err = f.WriteString(body)
	return errors.Join(err, f.Close())
}

func prepareSystemdUnits(spec *proto.LaunchSpec, installation *systemdInstallation, token string) error {
	groups := "0"
	if len(guestSlots.disks) != 0 {
		groups = "1"
	}
	flavor := "provided"
	if installation.runtime {
		flavor = "runtime"
	}
	unit := fmt.Sprintf(`[Unit]
Description=Sandbox execution service
DefaultDependencies=no
Conflicts=shutdown.target
Before=shutdown.target umount.target
IgnoreOnIsolate=yes
RefuseManualStart=yes
RefuseManualStop=yes
[Service]
Type=notify
NotifyAccess=main
ExecStart=%s systemd-executor %s %s %s
Restart=no
KillMode=mixed
Delegate=yes
TasksMax=infinity
OOMPolicy=continue
TimeoutStartSec=infinity
TimeoutStopSec=%ds
StandardInput=null
StandardOutput=tty
StandardError=inherit
TTYPath=/dev/console
`, systemdExecutorPath, token, groups, flavor, int64((systemdGrace(spec)+5*time.Second)/time.Second))
	if err := writeReserved("/run/systemd/system/"+systemdService, unit); err != nil {
		return err
	}
	if err := writeReserved("/run/systemd/system/"+systemdTarget, "[Unit]\nDescription=Sandbox environment\nDefaultDependencies=no\nRequires="+systemdService+" default.target\nAfter="+systemdService+"\n"); err != nil {
		return err
	}
	if err := prepareSystemdBootBarrier(
		"/run/systemd/system",
		"/etc/systemd/system.control",
		"/run/systemd/system.control",
		"/run/systemd/transient",
		"/run/systemd/generator.early",
		"/etc/systemd/system",
		"/etc/systemd/system.attached",
		"/run/systemd/system.attached",
		"/run/systemd/generator",
		systemdPrefix+"/lib/systemd/system",
		"/usr/local/lib/systemd/system",
		"/usr/lib/systemd/system",
		"/lib/systemd/system",
		"/run/systemd/generator.late",
	); err != nil {
		return err
	}
	// Protect actual platform mounts and their mounted parents. A path is not
	// necessarily a mount, and the delegation symlink must never become one.
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	paths := []string{systemdExecutorPath, systemdControl, cgroupMountPoint, "/dev/console", "/proc"}
	if installation.runtime {
		paths = append(paths, systemdPrefix)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		mount := fields[4]
		// systemd refuses .mount units for API filesystems. Their lifetime is
		// owned by its native API-mount/shutdown logic; drop-ins cannot turn
		// them into ordinary managed mounts. Only protect actual non-API units.
		if mount == "/" || systemdAPIMount(mount) {
			continue
		}
		for _, target := range paths {
			if target != mount && !strings.HasPrefix(target, mount+"/") {
				continue
			}
			name := systemdUnitName(mount)
			if seen[name] {
				break
			}
			seen[name] = true
			if err := writeReserved("/run/systemd/system/"+name+".d/50-sandbox-lifetime.conf", "[Unit]\nDefaultDependencies=no\nIgnoreOnIsolate=yes\nRefuseManualStop=yes\n"); err != nil {
				return err
			}
			installation.protected = append(installation.protected, name)
			break
		}
	}
	return nil
}

func systemdAPIMount(path string) bool {
	switch path {
	case "/proc", "/sys", "/dev", "/dev/pts", "/dev/shm", "/run", "/sys/fs/cgroup", "/dev/console", "/proc/kmsg", "/proc/sys", "/proc/sys/kernel/random/boot_id", "/sys/fs/selinux", "/run/host":
		return true
	}
	return strings.HasPrefix(path, "/sys/fs/cgroup/") || strings.HasPrefix(path, "/run/host/")
}

func systemdManagerEnv(installation *systemdInstallation) []string {
	env := append(os.Environ(), "container=sandbox", "SYSTEMD_LOG_TARGET=console")
	if installation.runtime {
		env = append(env,
			"SYSTEMD_GETTY_AUTO=no",
			"SYSTEMD_UNIT_PATH=/etc/systemd/system.control:/run/systemd/system.control:/run/systemd/transient:/run/systemd/generator.early:/etc/systemd/system:/etc/systemd/system.attached:/run/systemd/system:/run/systemd/system.attached:/run/systemd/generator:"+systemdPrefix+"/lib/systemd/system:/usr/local/lib/systemd/system:/usr/lib/systemd/system:/lib/systemd/system:/run/systemd/generator.late",
			"SYSTEMD_GENERATOR_PATH=/run/systemd/system-generators:/etc/systemd/system-generators:"+systemdPrefix+"/lib/systemd/system-generators:/usr/local/lib/systemd/system-generators:/usr/lib/systemd/system-generators:/lib/systemd/system-generators",
			"SYSTEMD_ENVIRONMENT_GENERATOR_PATH=/run/systemd/system-environment-generators:/etc/systemd/system-environment-generators:"+systemdPrefix+"/lib/systemd/system-environment-generators:/usr/local/lib/systemd/system-environment-generators:/usr/lib/systemd/system-environment-generators:/lib/systemd/system-environment-generators")
	}
	return env
}

func startSystemdExecutor(spec *proto.LaunchSpec, installation *systemdInstallation) (_ *executorClient, _ *exec.Cmd, resultErr error) {
	if err := prepareSystemdCgroupRoot(); err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(systemdControl, 0o700); err != nil {
		return nil, nil, fmt.Errorf("reserve %s: %w", systemdControl, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, nil, err
	}
	token := hex.EncodeToString(buf)
	if err := prepareSystemdUnits(spec, installation, token); err != nil {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: systemdControl + "/executor.sock", Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	defer listener.Close()
	parentSync, childSync, err := newChildSyncPair()
	if err != nil {
		return nil, nil, err
	}
	attr := &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS}
	if err := cgroupConfigureClone(attr, true); err != nil {
		_ = parentSync.Close()
		_ = childSync.Close()
		return nil, nil, err
	}
	cmd := &exec.Cmd{Path: "/proc/self/exe", Args: []string{"sandbox-init", "systemd-bootstrap", installation.manager}, Env: systemdManagerEnv(installation), Dir: "/", Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, SysProcAttr: attr, ExtraFiles: []*os.File{childSync}}
	client := &executorClient{spec: spec, ended: make(chan struct{})}
	client.systemd = &systemdEnvironment{installation: installation, token: token, refresh: make(map[uint64]chan int)}
	_, err = coordinateChild(cmd, parentSync, childSync, func(int) error { client.process = cmd.Process; return nil }, func(pid int) error {
		if err := requireControllers(sandboxCgroupDir); err != nil {
			return err
		}
		return cgroupFinalizeBootstrap(pid)
	}, nil, childSyncExecEOF)
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil, nil, fmt.Errorf("systemd bootstrap: %w", err)
	}
	client.startProcessWait(cmd)
	logf("systemd manager exec completed pid=%d", cmd.Process.Pid)
	defer func() {
		if resultErr != nil {
			_ = cmd.Process.Kill()
			<-client.processDone
		}
	}()
	type accepted struct {
		conn *net.UnixConn
		err  error
	}
	accept := make(chan accepted, 1)
	go func() { conn, err := listener.AcceptUnix(); accept <- accepted{conn, err} }()
	var conn *net.UnixConn
	select {
	case a := <-accept:
		if a.err != nil {
			return nil, nil, a.err
		}
		conn = a.conn
		logf("systemd executor accepted")
	case <-client.processDone:
		return nil, nil, fmt.Errorf("systemd exited before executor attach: %v", client.processErr)
	}
	defer func() {
		if resultErr != nil {
			_ = conn.Close()
		}
	}()
	if err := client.acceptSystemdExecutor(conn); err != nil {
		return nil, nil, err
	}
	_ = listener.Close() // one executor per environment, including after failure
	logf("systemd executor attached manager_pid=%d executor_pid=%d", client.process.Pid, client.systemd.executor.Pid)
	return client, cmd, nil
}

// cgroupMount also supports direct delegation. Its direct-only init leaf must
// not be left behind as an unmanaged sibling in the manager's namespace.
func prepareSystemdCgroupRoot() error {
	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.inner {
		return errors.New("systemd bootstrap must originate in the outer")
	}
	if applicationCgroups.target != nil {
		_ = applicationCgroups.target.Close()
		applicationCgroups.target = nil
	}
	if err := os.Remove(sandboxInitCgroupDir); err != nil {
		return fmt.Errorf("remove direct bootstrap leaf: %w", err)
	}
	target, err := openCgroupDir(sandboxCgroupDir)
	if err != nil {
		return err
	}
	applicationCgroups.target = target
	return nil
}

func runSystemdBootstrap(manager string) {
	sync, err := childSyncFromFD(3)
	if err != nil {
		die("systemd bootstrap handshake: %v", err)
	}
	if err := sync.waitFor(childMsgStart); err != nil {
		sync.fail(err)
	}
	if os.Getpid() != 1 {
		sync.fail(errors.New("systemd bootstrap must be environment PID 1"))
	}
	if err := makeChildMountsRSlave(); err != nil {
		sync.fail(err)
	}
	if err := remountChildProc(); err != nil {
		sync.fail(err)
	}
	if err := cgroupMountScoped(); err != nil {
		sync.fail(err)
	}
	if err := os.Mkdir(cgroupMountPoint+"/init.scope", 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		sync.fail(err)
	}
	if err := os.WriteFile(cgroupMountPoint+"/init.scope/cgroup.procs", []byte("0"), 0); err != nil {
		sync.fail(err)
	}
	if err := sync.readyAndWait(); err != nil {
		sync.fail(err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		sync.fail(err)
	}
	if err := syscall.Exec(manager, []string{manager, "--system", "--unit=" + systemdTarget, "--log-target=console"}, os.Environ()); err != nil {
		sync.fail(err)
	}
}

func runSystemdService(token string, groups, runtimeTools bool) {
	runtimeSystemdTools = runtimeTools
	if os.Getpid() == 1 {
		die("systemd executor must be the reserved service, not PID 1")
	}
	logf("systemd executor starting pid=%d", os.Getpid())
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: systemdControl + "/executor.sock", Net: "unix"})
	if err != nil {
		die("systemd executor attach: %v", err)
	}
	logf("systemd executor connected outer")
	link := newExecutorLink(conn)
	innerEvents = link
	inner := &innerExecutor{}
	go link.serve(inner.handle)
	// Connect the error channel before fallible service setup. Missing
	// controllers must report their real error instead of leaving Accept
	// waiting for a service that already failed.
	fail := func(stage string, err error) {
		_, _ = link.request(executorMessage{Op: "systemd-failed", Error: stage + ": " + err.Error()}, -1)
		die("systemd executor %s: %v", stage, err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fail("subreaper", err)
	}
	if err := prepareSystemdDelegation(groups); err != nil {
		fail("delegation", err)
	}
	logf("systemd executor delegation ready")
	fd, err := connectSystemdManager(context.Background())
	if err != nil {
		fail("manager peer", err)
	}
	logf("systemd executor connected manager peer")
	_, err = link.call(context.Background(), executorMessage{Op: "systemd-attach", Token: token, PID: os.Getpid()}, fd)
	_ = unix.Close(fd)
	if err != nil {
		die("systemd executor verification: %v", err)
	}
	if err := systemdNotifyReady(); err != nil {
		die("systemd executor READY: %v", err)
	}
	logf("systemd executor READY sent")
	_, err = link.call(context.Background(), executorMessage{Op: "systemd-ready"}, -1)
	if err != nil {
		die("systemd READY confirmation: %v", err)
	}
	<-link.done
	die("outer systemd executor connection closed")
}

func systemdNotifyReady() error {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return errors.New("missing service notification socket")
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1\nSTATUS=Attached sandbox execution service"))
	return err
}

func requireControllers(dir string) error {
	b, err := os.ReadFile(dir + "/cgroup.controllers")
	if err != nil {
		return err
	}
	if missing := missingControllers([]string{"cpu", "io", "memory", "pids"}, strings.Fields(string(b))); len(missing) > 0 {
		return fmt.Errorf("systemd delegation %s missing controllers: %s", dir, strings.Join(missing, ","))
	}
	return nil
}

func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(p, "/") && filepath.Clean(p) == p {
			return p, nil
		}
	}
	return "", errors.New("no unified service cgroup")
}

func prepareSystemdDelegation(groups bool) error {
	cg, err := ownCgroup()
	if err != nil {
		return err
	}
	if cg == "/" || !strings.HasSuffix(cg, "/"+systemdService) {
		return fmt.Errorf("unexpected executor service cgroup %q", cg)
	}
	dir := cgroupMountPoint + cg
	if err := requireControllers(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir+"/init", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dir+"/init/cgroup.procs", []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return err
	}
	b, err := os.ReadFile(dir + "/cgroup.procs")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "" {
		return errors.New("executor delegation root is not empty")
	}
	if err := cgroupEnableAdvertisedControllers(dir); err != nil {
		return err
	}
	if err := requireControllers(dir + "/init"); err != nil {
		return err
	}
	root, err := openCgroupDir(dir)
	if err != nil {
		return err
	}
	target, err := openCgroupDir(dir + "/init")
	if err != nil {
		_ = root.Close()
		return err
	}
	if err := os.Symlink(dir, systemdControl+"/cgroup"); err != nil {
		_ = root.Close()
		_ = target.Close()
		return fmt.Errorf("reserve delegation alias: %w", err)
	}
	applicationCgroups.Lock()
	applicationCgroups.control, applicationCgroups.inner, applicationCgroups.processGroups = true, true, groups
	applicationCgroups.root, applicationCgroups.target = root, target
	applicationCgroups.Unlock()
	return nil
}
