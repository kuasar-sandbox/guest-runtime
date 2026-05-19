// sandbox-init is the guest PID 1 binary inside the sandbox VM.
//
// It runs three phases:
//   1. Mount /proc /sys /dev, wait for /dev/vda + /dev/vdb, mount
//      overlay (lower=blk0 erofs ro, upper=blk1 ext4 rw) at
//      /mnt/newroot, then MS_MOVE + chroot. **Then** bind+listen
//      AF_VSOCK :5000 — the host→guest reverse channel must be open
//      before we dial host:5000 (avoids the race with the host's ping
//      ticker that starts firing as soon as `launch` is written, see
//      docs/sandbox-runtime.md §4.4).
//   2. Connect over virtio-vsock (CID 2:5000) to sandbox-ctl, send
//      hello, receive the launch spec; apply network; set up the app's
//      stdio (a pty or stdin/stdout/stderr pipes per spec.Stdio); send
//      launch_ack{stdio}; THEN keep that connection — it becomes the
//      stdio MUX (pkg/sandbox/mux). Fork the user app with
//      CLONE_NEWPID|CLONE_NEWNS (+ setsid/ctty in tty mode), wire its
//      0/1/2 to the prepared fds, send app_started{pid}.
//   3. Supervise: in parallel, the vsock listener goroutine dispatches
//      host-initiated ping / restore / attach / quiesce; the signal loop
//      reaps children. On user-app exit we drain the stdio MUX, send
//      app_exited{code,term_signal}, then reboot.
//
// All work is done via syscalls; no busybox or external tools are
// included in sandbox-runtime.erofs.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/mux"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"golang.org/x/sys/unix"
)

const (
	devicePollInterval = 50 * time.Millisecond
	devicePollTimeout  = 10 * time.Second
	gracefulShutdown   = 10 * time.Second

	// memReportInterval is the period of the /proc/meminfo sampler
	// that feeds the host-side balloon controller. Matches the
	// host-side reconcile cadence; at this rate the worst-case
	// reclaim latency is ~2 × interval.
	memReportInterval = 5 * time.Second
)

func main() {
	// Re-entry as the user-app exec helper (after fork+clone in phase 2).
	// argv: [self, "exec-child", workdir, appPath, args...]
	if len(os.Args) >= 4 && os.Args[1] == "exec-child" {
		runExecChild(os.Args[2], os.Args[3], os.Args[4:], false)
		return
	}
	// Joined exec command (sandbox-ctl exec): forked by the exec-join
	// helper after it has entered the app's mount + pid namespaces.
	if len(os.Args) >= 4 && os.Args[1] == "exec-child-joined" {
		runExecChild(os.Args[2], os.Args[3], os.Args[4:], true)
		return
	}
	// nsenter helper for sandbox-ctl exec.
	// argv: [self, "exec-join", appPid, tty(0|1), cwd, argv0, args...]
	if len(os.Args) >= 6 && os.Args[1] == "exec-join" {
		runExecJoin(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6:])
		return
	}

	logf("sandbox-init starting (pid=%d)", os.Getpid())
	if os.Getpid() != 1 {
		die("must run as PID 1, got %d", os.Getpid())
	}

	if err := phase1MountAndPivot(); err != nil {
		die("phase 1 failed: %v", err)
	}

	// Reverse-channel listener is brought up **before** hello (§9.1.3).
	// The fd lives for the entire sandbox lifetime — listener goroutine
	// is spawned in phase 3 once we have the app pid; the bound socket
	// itself is created here so host ping that starts as soon as `launch`
	// is written never hits ECONNREFUSED.
	revFD, err := bindVsockListener(proto.LaunchPort)
	if err != nil {
		die("phase 1 vsock listen: %v", err)
	}

	spec, cs, bridge, err := phase2Launch()
	if err != nil {
		die("phase 2 launch handshake: %v", err)
	}

	appPid, err := phase2ForkApp(spec, cs)
	if err != nil {
		die("phase 2 fork: %v", err)
	}

	// Move the app tree into the `app` cgroup so quiesce can freeze it.
	// Fatal on failure: a pid left in the root cgroup would not freeze,
	// silently defeating the snapshot freeze (§3.4).
	if err := cgroupPlaceApp(appPid); err != nil {
		die("cgroup place app pid=%d: %v", appPid, err)
	}

	// Notify host before entering supervisor — best-effort short conn.
	if err := notifyAppStarted(appPid); err != nil {
		logf("warn: app_started notify failed (continuing): %v", err)
	}

	supervisor := &supervisorState{appPid: appPid, restart: spec.Restart, execReg: newExecRegistry()}

	// Reverse-channel dispatch goroutine. Lives until reboot. It carries
	// the consoleBridge so host-initiated restore / attach can swap a
	// fresh MUX session under the still-running app's stdio pumps, and
	// the supervisor so exec sessions can register their children.
	go serveReverseChannel(revFD, supervisor, bridge)

	// Memory reporter: feeds the host-side balloon controller with
	// /proc/meminfo snapshots so it can drive vm.resize. Replaces
	// virtio-balloon free-page-reporting (whose mmu_notifier traffic
	// starves this very vsock listener after ~16 s).
	go runMemReporter(memReportInterval)

	phase3Supervise(supervisor, bridge)
	// phase3Supervise does not return.
}

// phase1MountAndPivot mounts /proc /sys /dev, waits for vda/vdb,
// assembles the overlay, then MS_MOVEs the overlay to / via chroot.
func phase1MountAndPivot() error {
	for _, m := range []struct {
		source, target, fstype string
		flags                  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
	} {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			if errors.Is(err, unix.EBUSY) {
				continue
			}
			return fmt.Errorf("mount %s on %s: %w", m.source, m.target, err)
		}
	}

	if err := waitForDevice("/dev/vda", devicePollTimeout); err != nil {
		return fmt.Errorf("wait /dev/vda: %w", err)
	}
	if err := waitForDevice("/dev/vdb", devicePollTimeout); err != nil {
		return fmt.Errorf("wait /dev/vdb: %w", err)
	}

	if err := unix.Mount("/dev/vda", "/mnt/lower", "erofs", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount blk0 (erofs ro) on /mnt/lower: %w", err)
	}
	if err := unix.Mount("/dev/vdb", "/mnt/upper", "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount blk1 (ext4 rw) on /mnt/upper: %w", err)
	}
	if err := os.MkdirAll("/mnt/upper/upperdir", 0o755); err != nil {
		return fmt.Errorf("mkdir upperdir: %w", err)
	}
	if err := os.MkdirAll("/mnt/upper/workdir", 0o755); err != nil {
		return fmt.Errorf("mkdir workdir: %w", err)
	}

	overlayOpts := "lowerdir=/mnt/lower,upperdir=/mnt/upper/upperdir,workdir=/mnt/upper/workdir"
	if err := unix.Mount("overlay", "/mnt/newroot", "overlay", 0, overlayOpts); err != nil {
		return fmt.Errorf("mount overlay on /mnt/newroot: %w", err)
	}

	for _, dir := range []string{"/mnt/newroot/proc", "/mnt/newroot/sys", "/mnt/newroot/dev"} {
		_ = os.MkdirAll(dir, 0o755)
	}

	for _, src := range []struct{ from, to string }{
		{"/proc", "/mnt/newroot/proc"},
		{"/sys", "/mnt/newroot/sys"},
		{"/dev", "/mnt/newroot/dev"},
	} {
		if err := unix.Mount(src.from, src.to, "", unix.MS_MOVE, ""); err != nil {
			logf("warn: MS_MOVE %s -> %s: %v (continuing)", src.from, src.to, err)
		}
	}

	if err := unix.Chdir("/mnt/newroot"); err != nil {
		return fmt.Errorf("chdir newroot: %w", err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("MS_MOVE newroot -> /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot .: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// devpts: tty mode (consoleBridge.openPTY) opens /dev/ptmx, whose
	// open() handler in the kernel resolves to a devpts mount in the
	// caller's namespace — no mount, no pty. Cheap to mount always;
	// pipe mode just doesn't use it. Mounted in the post-chroot root
	// so the MS_MOVE'd /dev stays simple (no child mounts to drag along).
	// ptmxmode=0666 lets a non-root app open /dev/ptmx if needed.
	if err := os.MkdirAll("/dev/pts", 0o755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %w", err)
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666"); err != nil {
		return fmt.Errorf("mount devpts on /dev/pts: %w", err)
	}

	// cgroup v2 freezer: the freeze domain for the user-app process
	// tree (snapshot freeze/thaw, §3.4). Mounted post-chroot so the
	// path is stable; no controllers enabled.
	if err := cgroupMount(); err != nil {
		return fmt.Errorf("cgroup setup: %w", err)
	}

	return nil
}

// phase2Launch opens the one cold-start vsock connection, runs the launch
// handshake on it, sets up the app's stdio per the negotiated StdioSpec,
// then — instead of closing — keeps the connection and turns it into the
// stdio MUX (pkg/sandbox/mux):
//
//	guest → host: hello
//	host  → guest: launch{spec}
//	guest applies network; sets up app stdio (pty or pipes)
//	guest → host: launch_ack{stdio = what we established}
//	host  → guest: ack
//	... connection now speaks the framed MUX sub-protocol ...
//
// Returns the launch spec, the child's 0/1/2 fds, and the consoleBridge
// (already attached to the new session and pumping). The bridge's pump
// goroutines park until the MUX has a session, so starting them before
// the user app is forked is safe.
func phase2Launch() (*proto.LaunchSpec, childStdio, *consoleBridge, error) {
	fail := func(err error) (*proto.LaunchSpec, childStdio, *consoleBridge, error) {
		return nil, childStdio{}, nil, err
	}

	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return fail(fmt.Errorf("vsock dial host:%d: %w", proto.LaunchPort, err))
	}
	// Until the MUX takes ownership, close the conn on any error path.
	muxOwns := false
	defer func() {
		if !muxOwns {
			_ = conn.Close()
		}
	}()

	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeHello, Phase: "ready"}); err != nil {
		return fail(fmt.Errorf("send hello: %w", err))
	}
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		return fail(fmt.Errorf("read launch: %w", err))
	}
	if msg.Type != proto.TypeLaunch || msg.Launch == nil {
		return fail(fmt.Errorf("expected launch message, got %q", msg.Type))
	}
	if msg.Launch.Exec == "" {
		return fail(errors.New("launch spec missing exec"))
	}

	if msg.Launch.Network != nil {
		if err := applyNetwork(msg.Launch.Network); err != nil {
			return fail(fmt.Errorf("apply network: %w", err))
		}
	}

	cs, bridge, err := setupAppStdio(msg.Launch.Stdio)
	if err != nil {
		return fail(fmt.Errorf("setup app stdio: %w", err))
	}
	// v1: the guest honors whatever the host asked for, so the established
	// spec is the requested one verbatim.
	established := msg.Launch.Stdio

	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeLaunchAck, Stdio: &established}); err != nil {
		return fail(fmt.Errorf("send launch_ack: %w", err))
	}
	ack, err := proto.ReadMessage(conn)
	if err != nil {
		return fail(fmt.Errorf("read ack: %w", err))
	}
	if ack.Type != proto.TypeAck {
		return fail(fmt.Errorf("expected ack, got %q", ack.Type))
	}

	// Hand the connection to the MUX. Clear any handshake deadline first —
	// mux.Session relies on Close (not a deadline) to unblock its read loop.
	_ = conn.SetDeadline(time.Time{})
	sess := mux.NewSession(conn, streamSetFor(established), mux.Options{OnSetWinsize: bridge.onSetWinsize})
	bridge.attach(sess)
	bridge.start()

	muxOwns = true
	return msg.Launch, cs, bridge, nil
}

// phase2ForkApp re-execs ourselves with the "exec-child" sentinel argv in
// a new PID + mount namespace, wiring the app's 0/1/2 to the fds prepared
// by setupAppStdio. In tty mode the child also gets a fresh session with
// the pty slave (its fd 0) as controlling terminal, so the line discipline
// can deliver SIGINT / SIGWINCH to the app's process group. The re-exec'd
// child (runExecChild) remounts /proc and execs the user app.
func phase2ForkApp(spec *proto.LaunchSpec, cs childStdio) (int, error) {
	self := "/proc/self/exe"
	args := append([]string{self, "exec-child", spec.Workdir, spec.Exec}, spec.Args...)

	sysAttr := &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}
	if cs.tty {
		sysAttr.Setsid = true
		sysAttr.Setctty = true // Ctty defaults to 0 = Stdin = the pty slave
	}

	cmd := exec.Cmd{
		Path:        self,
		Args:        args,
		Env:         envSliceFromMap(spec.Env),
		Dir:         "/",
		Stdin:       cs.stdin,
		Stdout:      cs.stdout,
		Stderr:      cs.stderr,
		SysProcAttr: sysAttr,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Drop our copies of the child ends so EOF propagates once the app
	// closes its fds (the consoleBridge keeps the opposite ends).
	_ = cs.stdin.Close()
	if cs.stdout != cs.stdin {
		_ = cs.stdout.Close()
	}
	if cs.stderr != cs.stdin && cs.stderr != cs.stdout {
		_ = cs.stderr.Close()
	}
	return cmd.Process.Pid, nil
}

// runExecChild runs in the forked child after CLONE_NEWPID|CLONE_NEWNS.
// We are pid=1 in the new pid ns. Remount /proc so it reflects the
// new ns, chdir to workdir, then exec the real app.
//
// If appPath has no '/', resolve via PATH lookup (image config Cmd
// often holds bare names like "python3" or "node", expecting standard
// PATH search semantics like sh/cmd would do).
func runExecChild(workdir, appPath string, args []string, joined bool) {
	// Independent children (the user app) get a fresh pid namespace and
	// need their own procfs. A joined exec command already runs in the
	// app's mount + pid namespace, where /proc is mounted for that pid
	// ns — remounting it would disrupt the shared view.
	if !joined {
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			if errRemount := unix.Mount("none", "/proc", "", unix.MS_REMOUNT, ""); errRemount != nil {
				die("exec-child: remount /proc: %v / %v", err, errRemount)
			}
		}
	}

	if workdir != "" && workdir != "/" {
		if err := unix.Chdir(workdir); err != nil {
			die("exec-child: chdir %s: %v", workdir, err)
		}
	}

	resolved := appPath
	if !strings.Contains(appPath, "/") {
		// PATH lookup uses os.Environ() (set by parent from spec.Env).
		p, err := exec.LookPath(appPath)
		if err != nil {
			die("exec-child: %s not found in PATH: %v", appPath, err)
		}
		resolved = p
	}

	// Tie the command's lifetime to the exec-join helper (our parent).
	// Done here, from inside the app's pid namespace, rather than via
	// syscall.SysProcAttr.Pdeathsig in runExecJoin — Go's ForkExec
	// child self-check (getppid vs pre-clone parent pid) misfires across
	// setns(CLONE_NEWPID) and would SIGKILL us at startup. The kernel
	// tracks the real parent task regardless of pid-ns visibility, and
	// PR_SET_PDEATHSIG survives a normal (non-setuid) execve, so it
	// still fires for the real command when the helper dies.
	if joined {
		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
			die("exec-child: set pdeathsig: %v", err)
		}
	}

	if err := syscall.Exec(resolved, append([]string{appPath}, args...), os.Environ()); err != nil {
		die("exec-child: exec %s: %v", resolved, err)
	}
}

// supervisorState is shared between the signal loop and the reverse
// channel goroutine.
type supervisorState struct {
	appPid  int
	restart string
	execReg *execRegistry // exec-session child reaping + quiesce gating
}

// phase3Supervise reaps children. On user-app exit it drains the stdio
// MUX, notifies the host (best-effort), then reboots.
func phase3Supervise(s *supervisorState, b *consoleBridge) {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGCHLD:
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
				logf("reaped pid=%d exit=%d signal=%v", pid, status.ExitStatus(), status.Signal())
				if pid == s.appPid {
					handleAppExit(status, s, b)
					return
				}
				// Non-app child = an exec session's child. Route its
				// status to the waiting session goroutine (it must not
				// trigger the app-exit reboot).
				s.execReg.deliver(pid, status)
			}
		case syscall.SIGTERM, syscall.SIGINT:
			logf("received %v, sending SIGTERM to app pid=%d", sig, s.appPid)
			_ = syscall.Kill(s.appPid, syscall.SIGTERM)
			waitOrTimeout(s.appPid, gracefulShutdown)
			b.appExited()
			notifyAppExited(0, 0)
			doReboot()
			return
		}
	}
}

func handleAppExit(status syscall.WaitStatus, s *supervisorState, b *consoleBridge) {
	var code, sig int
	if status.Signaled() {
		sig = int(status.Signal())
		code = 128 + sig // shell 128+signo convention
	} else {
		code = status.ExitStatus()
	}
	logf("app exited code=%d signal=%d (restart=%s); draining MUX, notifying host, rebooting", code, sig, s.restart)
	b.appExited() // close app fds, flush guest→host streams (bounded)
	notifyAppExited(code, sig)
	// v1: all restart policies just reboot. Real in-place restart is v2.
	doReboot()
}

func waitOrTimeout(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err != nil || wpid == pid {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	logf("graceful shutdown timed out, sending SIGKILL")
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func doReboot() {
	// POWER_OFF (not RESTART): the sandbox model is one-shot — when
	// the user app exits, the sandbox is done and CH should exit too.
	// LINUX_REBOOT_CMD_RESTART triggers CH's "reboot in place" flow,
	// which tries to reconnect vhost-user-blk backends. Our backends
	// only accept one connection (sandbox = single VM lifetime), so
	// reconnect fails and CH exits non-zero. POWER_OFF cleanly signals
	// vCPU shutdown; CH exits 0.
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		die("reboot: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(devicePollInterval)
	}
	return fmt.Errorf("device %s did not appear within %s", path, timeout)
}

func envSliceFromMap(m map[string]string) []string {
	if len(m) == 0 {
		return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// runMemReporter samples /proc/meminfo every interval and pushes a
// mem_report to the host. Best-effort: errors logged, ticker continues.
// First report fires immediately so the host's BalloonController gets
// a baseline before its first reconcile tick.
func runMemReporter(interval time.Duration) {
	push := func() {
		avail, total, err := readMemInfo()
		if err != nil {
			logf("mem_report: read /proc/meminfo: %v", err)
			return
		}
		if err := notifyMemReport(avail, total); err != nil {
			logf("mem_report: %v", err)
		}
	}
	push()
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		push()
	}
}

// readMemInfo parses /proc/meminfo's MemTotal and MemAvailable in
// bytes. Linux reports kB; we shift to bytes for the wire protocol.
func readMemInfo() (memAvailable, memTotal uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = parseMemInfoKB(line) << 10
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvailable = parseMemInfoKB(line) << 10
		}
		if memTotal != 0 && memAvailable != 0 {
			break
		}
	}
	if memTotal == 0 {
		return 0, 0, errors.New("MemTotal not found")
	}
	return memAvailable, memTotal, nil
}

// parseMemInfoKB pulls the kB-scale integer out of a /proc/meminfo
// line of shape "MemTotal:    8064212 kB". Returns 0 on malformed input.
func parseMemInfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// initStart is the wall clock at sandbox-init main() entry. Used to
// prefix every log line with elapsed milliseconds — visible from the
// host-side guest console capture, gives a millisecond-resolution view
// of guest cold-start phases that complements sandbox-ctl's
// host-side microsecond timestamps.
var initStart = time.Now()

func logf(format string, args ...any) {
	elapsedMs := time.Since(initStart).Milliseconds()
	fmt.Fprintf(os.Stderr, "[sandbox-init][T+%dms] "+format+"\n",
		append([]any{elapsedMs}, args...)...)
}

func die(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}
