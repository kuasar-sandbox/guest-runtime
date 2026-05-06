package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// LaunchServer accepts the guest's hello on a UDS that cloud-hypervisor
// proxies from vsock CID=2 port=LaunchPort, sends back the launch spec,
// then closes the connection.
//
// **Short-lived handshake by design**: holding the connection open into
// phase 3 of sandbox-init would leave an active vsock socket in any
// subsequent VM snapshot, polluting both guest TCP-style state and the
// host's vsock proxy bookkeeping. Snapshots are taken cleanest when no
// inflight control connections exist.
//
// Naming convention follows cloud-hypervisor's hybrid vsock: when the
// guest connects to vsock host:<port>, CH proxies to "<base>_<port>" on
// the host. base is given as --vsock socket=<base>; the port suffix
// is computed by us using proto.LaunchPort.
type LaunchServer struct {
	Path     string           // /run/<sid>/vsock.sock_5000
	Spec     *proto.LaunchSpec
	Logf     func(string, ...any)
	listener *net.UnixListener
	stopOnce sync.Once
	stopped  chan struct{}
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

// Serve accepts one connection from sandbox-init, performs the hello /
// launch handshake, then closes the connection. Subsequent connections
// (e.g. for hypothetical status messages) would be separate dials —
// kept out of v1 to preserve snapshot cleanliness.
func (s *LaunchServer) Serve(ctx context.Context) error {
	defer s.cleanup()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	conn, err := s.listener.AcceptUnix()
	if err != nil {
		select {
		case <-s.stopped:
			return nil
		default:
			return fmt.Errorf("launchsrv: accept: %w", err)
		}
	}
	defer conn.Close()
	s.Logf("launch: guest connected from %s", conn.RemoteAddr())

	hello, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("launchsrv: read hello: %w", err)
	}
	if hello.Type != proto.TypeHello {
		return fmt.Errorf("launchsrv: expected hello, got %q", hello.Type)
	}
	s.Logf("launch: hello received (phase=%q), sending launch spec", hello.Phase)

	if err := proto.WriteMessage(conn, &proto.Message{
		Type:   proto.TypeLaunch,
		Launch: s.Spec,
	}); err != nil {
		return fmt.Errorf("launchsrv: send launch: %w", err)
	}

	s.Logf("launch: launch spec sent; closing handshake connection")
	return nil
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
