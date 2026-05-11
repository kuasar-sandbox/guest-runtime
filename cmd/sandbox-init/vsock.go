package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"golang.org/x/sys/unix"
)

// Vsock dial uses exponential backoff because sandbox boot latency is
// the project's primary metric — every additional millisecond on the
// cold-start path matters. Start tight (microsecond grain) so the first
// ECONNREFUSED is followed almost immediately by a retry, then ramp
// until we hit a 10 ms cap. Total budget bounded by vsockDialDeadline.
const (
	vsockDialStart    = 100 * time.Microsecond
	vsockDialMax      = 10 * time.Millisecond
	vsockDialDeadline = 5 * time.Second
)

// vsockConn wraps an AF_VSOCK SOCK_STREAM fd as io.ReadWriteCloser
// implementing the deadline-aware net.Conn surface that
// proto.{Read,Write}Message needs.
type vsockConn struct {
	fd int
}

func (c *vsockConn) Read(b []byte) (int, error)  { return syscall.Read(c.fd, b) }
func (c *vsockConn) Write(b []byte) (int, error) { return syscall.Write(c.fd, b) }
func (c *vsockConn) Close() error                { return syscall.Close(c.fd) }

// SetDeadline applies SO_RCVTIMEO + SO_SNDTIMEO. AF_VSOCK supports both
// (kernel >= 5.16). Coarse-grained but adequate for our ms-level
// per-message budgets.
func (c *vsockConn) SetDeadline(t time.Time) error {
	d := time.Until(t)
	if d < 0 {
		d = 1 * time.Microsecond
	}
	tv := unix.NsecToTimeval(d.Nanoseconds())
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("SO_RCVTIMEO: %w", err)
	}
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		return fmt.Errorf("SO_SNDTIMEO: %w", err)
	}
	return nil
}

// dialVsock opens an AF_VSOCK SOCK_STREAM socket and connects to (cid,
// port). The first attempt fires immediately; on ECONNREFUSED (host
// listener not yet ready) we exponentially back off from vsockDialStart
// up to vsockDialMax until vsockDialDeadline elapses.
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

// bindVsockListener creates an AF_VSOCK SOCK_STREAM socket bound to
// (CID=any, port). Returns the listening fd. Must be called after
// phase 1 chroot (the syscall itself doesn't depend on rootfs but
// keeping a single ordering rule simplifies reasoning).
func bindVsockListener(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	// VMADDR_CID_ANY = 0xffffffff means "accept on any local CID".
	addr := &unix.SockaddrVM{CID: 0xffffffff, Port: port}
	if err := unix.Bind(fd, addr); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
	}
	return fd, nil
}

// serveReverseChannel accepts host-initiated connections on the bound
// vsock listener and dispatches each in its own goroutine. The handler
// reads one request, dispatches by type, writes one response, closes.
//
// Listener fd lives for the entire sandbox lifetime and is **never
// closed by quiesce** (§9.1.5) — closing it would cut off the host's
// subsequent restore notification.
//
// sup is currently unused but reserved: future restore-side hooks
// (e.g. re-fork on agent crash) need supervisor state.
func serveReverseChannel(listenFD int, sup *supervisorState) {
	_ = sup
	for {
		nfd, _, err := unix.Accept(listenFD)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			logf("reverse-channel: accept error %v (continuing)", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go handleReverseConn(nfd)
	}
}

// sup currently unused; kept on the goroutine signature to leave room
// for restore-side hooks that need supervisor state (e.g. re-fork).
func handleReverseConn(fd int) {
	c := &vsockConn{fd: fd}
	defer c.Close()

	// Per-conn deadline tight enough that a stuck host can't pin a
	// goroutine forever, generous enough for quiesce's drop_caches
	// path (which can take tens of ms on a busy ext4).
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	req, err := proto.ReadMessage(c)
	if err != nil {
		logf("reverse-channel: read: %v", err)
		return
	}
	switch req.Type {
	case proto.TypePing:
		// Echo id + t_send_ns; host computes RTT.
		resp := &proto.Message{Type: proto.TypePong, ID: req.ID, TSendNs: req.TSendNs}
		if err := proto.WriteMessage(c, resp); err != nil {
			logf("reverse-channel: write pong: %v", err)
		}

	case proto.TypeRestore:
		logf("reverse-channel: restore epoch=%d", req.Epoch)
		// v1: just ack. Future hooks (re-seed RNG, replay timer, etc.)
		// fit here.
		resp := &proto.Message{Type: proto.TypeRestored, Epoch: req.Epoch}
		if err := proto.WriteMessage(c, resp); err != nil {
			logf("reverse-channel: write restored: %v", err)
		}

	case proto.TypeQuiesce:
		logf("reverse-channel: quiesce — running pre-snapshot cleanup")
		runQuiesce()
		resp := &proto.Message{Type: proto.TypeQuiesced}
		if err := proto.WriteMessage(c, resp); err != nil {
			logf("reverse-channel: write quiesced: %v", err)
		}

	default:
		logf("reverse-channel: unknown type %q", req.Type)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "unknown type"})
	}
}

// notifyAppStarted dials the host launch UDS and sends a short-conn
// app_started notification. Best-effort: errors are logged but not
// fatal — the host has fallback signals (ping, ctl.sock).
func notifyAppStarted(pid int) error {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if vc, ok := conn.(*vsockConn); ok {
		_ = vc.SetDeadline(time.Now().Add(proto.DeadlineAppNotify))
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAppStarted, PID: pid}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.Type != proto.TypeAck {
		return fmt.Errorf("unexpected response %q", resp.Type)
	}
	return nil
}

// notifyMemReport dials the host launch UDS and pushes a /proc/meminfo
// snapshot. Best-effort: errors logged, the next ticker iteration tries
// again. Used by the host-side balloon controller (replaces
// virtio-balloon free-page-reporting, see docs/sandbox.md §known-issues).
func notifyMemReport(memAvailable, memTotal uint64) error {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if vc, ok := conn.(*vsockConn); ok {
		_ = vc.SetDeadline(time.Now().Add(proto.DeadlineAppNotify))
	}
	if err := proto.WriteMessage(conn, &proto.Message{
		Type:              proto.TypeMemReport,
		MemAvailableBytes: memAvailable,
		MemTotalBytes:     memTotal,
	}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.Type != proto.TypeMemReportAck {
		return fmt.Errorf("unexpected response %q", resp.Type)
	}
	return nil
}

// notifyAppExited dials the host launch UDS and sends a short-conn
// app_exited notification. Best-effort — guest reboots regardless of
// outcome (§9.1.6).
func notifyAppExited(code int) {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		logf("app_exited notify: dial: %v (continuing reboot)", err)
		return
	}
	defer conn.Close()
	if vc, ok := conn.(*vsockConn); ok {
		_ = vc.SetDeadline(time.Now().Add(proto.DeadlineAppNotify))
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAppExited, Code: code}); err != nil {
		logf("app_exited notify: write: %v", err)
		return
	}
	if _, err := proto.ReadMessage(conn); err != nil {
		logf("app_exited notify: read ack: %v (continuing reboot)", err)
	}
}
