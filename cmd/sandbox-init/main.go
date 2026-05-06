// sandbox-init is the guest PID 1 binary inside the sandbox VM.
//
// It runs three phases:
//   1. Mount /proc /sys /dev, wait for /dev/vda + /dev/vdb, mount overlay
//      (lower=blk0 erofs ro, upper=blk1 ext4 rw) at /mnt/newroot, then
//      MS_MOVE + chroot to make the overlay the new root.
//   2. Connect over virtio-vsock (CID 2:5000) to sandbox-ctl, send hello,
//      receive the launch spec (exec/args/env/workdir/restart), then fork
//      a child with CLONE_NEWPID|CLONE_NEWNS, in the child remount /proc
//      and exec the user app.
//   3. Supervise: reap children, on user app exit reboot the VM.
//
// All work is done via syscalls; no busybox or external tools are
// included in sandbox-runtime.erofs.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"golang.org/x/sys/unix"
)

const (
	devicePollInterval = 50 * time.Millisecond
	devicePollTimeout  = 10 * time.Second
	gracefulShutdown   = 10 * time.Second

	// Vsock dial uses exponential backoff because sandbox boot latency
	// is the project's primary metric — every additional millisecond on
	// the cold-start path matters. Start tight (microsecond grain) so
	// the first ECONNREFUSED is followed almost immediately by a retry,
	// then ramp until we hit a 10ms cap. Total budget bounded by
	// vsockDialDeadline.
	vsockDialStart    = 100 * time.Microsecond
	vsockDialMax      = 10 * time.Millisecond
	vsockDialDeadline = 5 * time.Second
)

func main() {
	// Re-entry as the user-app exec helper (after fork+clone in phase 2).
	// argv: [self, "exec-child", workdir, appPath, args...]
	if len(os.Args) >= 4 && os.Args[1] == "exec-child" {
		runExecChild(os.Args[2], os.Args[3], os.Args[4:])
		return
	}

	logf("sandbox-init starting (pid=%d)", os.Getpid())
	if os.Getpid() != 1 {
		die("must run as PID 1, got %d", os.Getpid())
	}

	if err := phase1MountAndPivot(); err != nil {
		die("phase 1 failed: %v", err)
	}
	logf("phase 1 done: rootfs assembled")

	spec, err := phase2ReceiveLaunch()
	if err != nil {
		die("phase 2 receive: %v", err)
	}
	logf("phase 2: received launch spec exec=%s args=%v restart=%s workdir=%s",
		spec.Exec, spec.Args, spec.Restart, spec.Workdir)

	if spec.Network != nil {
		if err := applyNetwork(spec.Network); err != nil {
			die("phase 2 network: %v", err)
		}
		logf("phase 2: network applied iface=%s ip=%s gw=%s hostname=%s",
			spec.Network.Interface, spec.Network.IPCIDR,
			spec.Network.Gateway, spec.Network.Hostname)
	}

	appPid, err := phase2ForkApp(spec)
	if err != nil {
		die("phase 2 fork: %v", err)
	}
	logf("phase 2 done: app pid=%d", appPid)

	phase3Supervise(appPid, spec)
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

	return nil
}

// phase2ReceiveLaunch opens a vsock connection to the host, exchanges
// hello → launch with sandbox-ctl, and **closes the connection** before
// returning. Holding the channel open through phase 3 would leave a
// live vsock socket in any subsequent VM snapshot, polluting both guest
// and host VMM state — short-lived handshake keeps snapshots clean.
func phase2ReceiveLaunch() (*proto.LaunchSpec, error) {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return nil, fmt.Errorf("vsock dial host:%d: %w", proto.LaunchPort, err)
	}
	// Closed before returning — single-shot handshake.
	defer conn.Close()

	if err := proto.WriteMessage(conn, &proto.Message{
		Type:  proto.TypeHello,
		Phase: "ready",
	}); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}

	msg, err := proto.ReadMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("read launch: %w", err)
	}
	if msg.Type != proto.TypeLaunch || msg.Launch == nil {
		return nil, fmt.Errorf("expected launch message, got %q", msg.Type)
	}
	if msg.Launch.Exec == "" {
		return nil, errors.New("launch spec missing exec")
	}
	return msg.Launch, nil
}

// vsockConn wraps an AF_VSOCK SOCK_STREAM fd as io.ReadWriteCloser.
// Go's stdlib net package doesn't recognize AF_VSOCK (net.FileConn
// returns "protocol not supported"), so we drive the syscall directly.
type vsockConn struct{ fd int }

func (c *vsockConn) Read(b []byte) (int, error)  { return syscall.Read(c.fd, b) }
func (c *vsockConn) Write(b []byte) (int, error) { return syscall.Write(c.fd, b) }
func (c *vsockConn) Close() error                { return syscall.Close(c.fd) }

// dialVsock opens an AF_VSOCK SOCK_STREAM socket and connects to (cid,
// port). The first attempt fires immediately; on ECONNREFUSED (host
// listener not yet ready) we exponentially back off from vsockDialStart
// up to vsockDialMax until vsockDialDeadline elapses.
//
// Boot-latency sensitive: each microsecond on the cold-start critical
// path matters, so the early backoff stays sub-millisecond.
func dialVsock(cid, port uint32) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(vsockDialDeadline)
	delay := vsockDialStart
	var lastErr error
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
		if err != nil {
			return nil, fmt.Errorf("socket: %w", err)
		}
		err = unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
		if err == nil {
			return &vsockConn{fd: fd}, nil
		}
		_ = unix.Close(fd)
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial deadline %s exceeded: %w", vsockDialDeadline, lastErr)
		}
		time.Sleep(delay)
		if delay < vsockDialMax {
			delay *= 2
			if delay > vsockDialMax {
				delay = vsockDialMax
			}
		}
	}
}

// phase2ForkApp re-execs ourselves with "exec-child" sentinel argv,
// in a new PID + Mount namespace. The re-exec'd child remounts /proc
// and execs the user app per the LaunchSpec.
func phase2ForkApp(spec *proto.LaunchSpec) (int, error) {
	self := "/proc/self/exe"

	args := []string{self, "exec-child", spec.Workdir, spec.Exec}
	args = append(args, spec.Args...)

	cmd := exec.Cmd{
		Path:   self,
		Args:   args,
		Env:    envSliceFromMap(spec.Env),
		Dir:    "/",
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		},
	}
	if err := cmd.Start(); err != nil {
		return 0, err
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
func runExecChild(workdir, appPath string, args []string) {
	logf("exec-child pid=%d (in new pid+mount ns) workdir=%s app=%s",
		os.Getpid(), workdir, appPath)

	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if errRemount := unix.Mount("none", "/proc", "", unix.MS_REMOUNT, ""); errRemount != nil {
			die("exec-child: remount /proc: %v / %v", err, errRemount)
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

	if err := syscall.Exec(resolved, append([]string{appPath}, args...), os.Environ()); err != nil {
		die("exec-child: exec %s: %v", resolved, err)
	}
}

// phase3Supervise reaps children and reboots the VM when the user app
// exits or we receive SIGTERM/SIGINT.
func phase3Supervise(appPid int, spec *proto.LaunchSpec) {
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
				if pid == appPid {
					handleAppExit(status, spec)
					return
				}
			}
		case syscall.SIGTERM, syscall.SIGINT:
			logf("received %v, sending SIGTERM to app pid=%d", sig, appPid)
			_ = syscall.Kill(appPid, syscall.SIGTERM)
			waitOrTimeout(appPid, gracefulShutdown)
			doReboot()
			return
		}
	}
}

func handleAppExit(status syscall.WaitStatus, spec *proto.LaunchSpec) {
	code := status.ExitStatus()
	logf("app exited code=%d (restart=%s); rebooting VM", code, spec.Restart)
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
	logf("calling reboot syscall (POWER_OFF — sandbox lifecycle ends)")
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
