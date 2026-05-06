package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// Server runs the ctl.sock listener inside a sandbox-ctl run process.
// Currently handles snapshot_request only.
type Server struct {
	Path    string
	Handler func(req Request) (Response, error)
	Logf    func(string, ...any)

	listener *net.UnixListener
	stopOnce sync.Once
	stopped  chan struct{}
}

// Listen binds the UDS.
func (s *Server) Listen() error {
	if s.Handler == nil {
		return errors.New("snapshot: Server.Handler is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("ctl.sock resolve: %w", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("ctl.sock listen: %w", err)
	}
	s.listener = l
	return nil
}

// Serve accepts connections until ctx cancels or Stop is called.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("snapshot: Listen not called")
	}
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				return nil
			default:
				return fmt.Errorf("ctl.sock accept: %w", err)
			}
		}
		go s.handle(conn)
	}
}

// Stop closes the listener and removes the socket file.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.Path)
	})
}

func (s *Server) handle(conn *net.UnixConn) {
	defer conn.Close()
	var req Request
	if err := ReadMessage(conn, &req); err != nil {
		s.Logf("ctl.sock: read: %v", err)
		_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
		return
	}
	if req.Type != TypeSnapshotRequest {
		_ = WriteMessage(conn, Response{Type: TypeError, Msg: "unknown type: " + req.Type})
		return
	}
	resp, err := s.Handler(req)
	if err != nil {
		_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
		return
	}
	resp.Type = TypeSnapshotDone
	if err := WriteMessage(conn, resp); err != nil {
		s.Logf("ctl.sock: write resp: %v", err)
	}
}
