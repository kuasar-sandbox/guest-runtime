package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// LaunchServer accepts the guest's launch-channel connections on a UDS
// that cloud-hypervisor's hybrid vsock proxies from CID=2 port=5000.
//
// The protocol (docs/sandbox-runtime.md §4) is bidirectional and
// short-lived. This server handles the **guest → host** direction:
//
//   - hello       → server replies with the LaunchSpec (one-shot)
//   - launch_ack  → server acks; OnLaunchAck callback fires (Settled trigger)
//   - app_started → server acks; OnAppStarted callback fires
//   - app_exited  → server acks; OnAppExited callback fires
//
// Each connection carries exactly one request + one response, then both
// sides close. Multiple goroutines serve concurrent connections — the
// guest sandbox-init may emit app_started while a (rare) hello retry is
// still in flight, and we don't want one to block the other.
//
// Naming convention follows cloud-hypervisor's hybrid vsock: when the
// guest connects to vsock host:<port>, CH proxies to "<base>_<port>" on
// the host. base is given as --vsock socket=<base>; the port suffix
// is computed by us using proto.LaunchPort.
type LaunchServer struct {
	Path string
	Spec *proto.LaunchSpec
	Logf func(string, ...any)

	// OnLaunchAck fires when the guest reports it has applied the launch
	// spec (network configured, ready to fork the app). Optional; nil →
	// just ack. This is the Settled trigger: post-boot transient is over,
	// safe to engage memory.high / controller RPCs.
	OnLaunchAck func()

	// OnAppStarted fires when the guest reports its user app has been
	// fork/execed. Optional; nil → just ack.
	OnAppStarted func(pid int)

	// OnAppExited fires when the guest reports its user app has
	// exited. Optional; nil → just ack.
	OnAppExited func(code int)

	listener      *net.UnixListener
	stopOnce      sync.Once
	stopped       chan struct{}
	helloDone     chan struct{}
	helloOnce     sync.Once
	helloSent     atomic.Bool
	launchAckDone chan struct{}
	launchAckOnce sync.Once
	connsWG       sync.WaitGroup
}

// Listen binds the UDS for the launch port. Must be called before CH
// spawns; otherwise the guest's first connect attempt fails (it will
// retry but warning will appear in logs).
func (s *LaunchServer) Listen() error {
	if s.Spec == nil {
		return errors.New("launchsrv: Spec is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	s.helloDone = make(chan struct{})
	s.launchAckDone = make(chan struct{})

	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("launchsrv: resolve %s: %w", s.Path, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("launchsrv: listen %s: %w", s.Path, err)
	}
	s.listener = l
	return nil
}

// Serve runs the accept loop until ctx is cancelled or Stop is called.
// Each accepted connection is handled in its own goroutine: read one
// message, dispatch, write the response, close.
func (s *LaunchServer) Serve(ctx context.Context) error {
	defer s.cleanup()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				s.connsWG.Wait()
				return nil
			default:
				return fmt.Errorf("launchsrv: accept: %w", err)
			}
		}
		s.connsWG.Add(1)
		go func() {
			defer s.connsWG.Done()
			defer conn.Close()
			s.handleConn(conn)
		}()
	}
}

// HelloDone returns a channel that closes once the hello/launch
// handshake has completed (LaunchSpec written, conn closed). Useful
// for callers that want to gate "ping ticker start" on this event.
func (s *LaunchServer) HelloDone() <-chan struct{} { return s.helloDone }

// LaunchAckDone returns a channel that closes once the guest has
// acknowledged that it has received the launch spec and applied
// network config (post-boot transient over). Settled gate.
func (s *LaunchServer) LaunchAckDone() <-chan struct{} { return s.launchAckDone }

func (s *LaunchServer) handleConn(conn *net.UnixConn) {
	// Bound the per-conn protocol exchange. Keeps a stuck guest from
	// pinning a goroutine forever.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	msg, err := proto.ReadMessage(conn)
	if err != nil {
		s.Logf("launch: read: %v", err)
		return
	}

	switch msg.Type {
	case proto.TypeHello:
		if s.helloSent.Load() {
			s.Logf("launch: duplicate hello rejected")
			_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "hello already served"})
			return
		}
		s.Logf("launch: hello received (phase=%q), sending launch spec", msg.Phase)
		if err := proto.WriteMessage(conn, &proto.Message{
			Type:   proto.TypeLaunch,
			Launch: s.Spec,
		}); err != nil {
			s.Logf("launch: send launch: %v", err)
			return
		}
		s.helloSent.Store(true)
		s.helloOnce.Do(func() { close(s.helloDone) })

		// Same-connection launch_ack: guest applies network, then sends
		// launch_ack on this conn. Reading it here (instead of accepting a
		// fresh conn) makes the post-boot transient boundary unambiguous —
		// OnLaunchAck fires only after the guest has actually applied the
		// spec.
		ack, err := proto.ReadMessage(conn)
		if err != nil {
			s.Logf("launch: read launch_ack: %v", err)
			return
		}
		if ack.Type != proto.TypeLaunchAck {
			s.Logf("launch: expected launch_ack, got %q", ack.Type)
			_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "expected launch_ack"})
			return
		}
		s.Logf("launch: launch_ack received")
		if s.OnLaunchAck != nil {
			s.OnLaunchAck()
		}
		s.launchAckOnce.Do(func() { close(s.launchAckDone) })
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})

	case proto.TypeLaunchAck:
		s.Logf("launch: launch_ack received")
		if s.OnLaunchAck != nil {
			s.OnLaunchAck()
		}
		s.launchAckOnce.Do(func() { close(s.launchAckDone) })
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})

	case proto.TypeAppStarted:
		s.Logf("launch: app_started pid=%d", msg.PID)
		if s.OnAppStarted != nil {
			s.OnAppStarted(msg.PID)
		}
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})

	case proto.TypeAppExited:
		s.Logf("launch: app_exited code=%d", msg.Code)
		if s.OnAppExited != nil {
			s.OnAppExited(msg.Code)
		}
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})

	default:
		s.Logf("launch: unknown message type %q", msg.Type)
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "unknown type"})
	}
}

// Stop terminates the accept loop. Safe to call multiple times.
func (s *LaunchServer) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *LaunchServer) cleanup() {
	_ = os.Remove(s.Path)
}
