package uffd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// VAReportServer accepts a single CH-side va_report message + the uffd
// fd attached via SCM_RIGHTS, registers ProcessCH in the AddressMap,
// and signals OnReady (if non-nil) with the uffd fd for handler
// construction. After ack, CH proceeds to vCPU run.
//
// Only one va_report is expected per sandbox lifecycle (single zone).
type VAReportServer struct {
	Path    string
	AddrMap *AddressMap
	Logf    func(string, ...any)

	// OnReady is invoked synchronously inside handle() right before
	// sending ack. The handler should adopt the uffd fd (the server
	// passes ownership) and start its goroutines. Returning an error
	// causes the va_report to be NAK'd and the fd closed.
	//
	// Typical impl: construct uffd.Handler via NewFromFD and Start().
	OnReady func(uffdFD int, vaStart, size uint64) error

	listener   *net.UnixListener
	acceptedMu sync.Mutex
	accepted   bool
	stopOnce   sync.Once
	stopped    chan struct{}
}

// Listen binds the UDS socket. Must be called before CH spawns; CH's
// patched create_ram_region will Dial this socket synchronously.
func (s *VAReportServer) Listen() error {
	if s.AddrMap == nil {
		return errors.New("vareport: AddrMap is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("vareport: resolve %s: %w", s.Path, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("vareport: listen %s: %w", s.Path, err)
	}
	s.listener = l
	return nil
}

// Serve runs the accept loop until ctx is cancelled or Stop is called.
// Returns nil on clean shutdown.
func (s *VAReportServer) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("vareport: Listen not called")
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
				return fmt.Errorf("vareport: accept: %w", err)
			}
		}
		s.handle(conn)
	}
}

// Stop closes the listener and unlinks the socket.
func (s *VAReportServer) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.Path)
	})
}

type vaReportMsg struct {
	Type    string `json:"type"`
	ZoneID  string `json:"zone_id,omitempty"`
	VAStart uint64 `json:"va_start"`
	Size    uint64 `json:"size"`
}

type ackMsg struct {
	Type string `json:"type"`
	Msg  string `json:"msg,omitempty"`
}

func (s *VAReportServer) handle(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	// Read u32 length prefix + JSON body, plus SCM_RIGHTS ancillary
	// containing the uffd fd in the same recvmsg call.
	body, fds, err := readLPJSONWithFDs(conn)
	if err != nil {
		s.Logf("vareport: read: %v", err)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
		closeAll(fds)
		return
	}
	defer closeAll(fds)
	var req vaReportMsg
	if err := json.Unmarshal(body, &req); err != nil {
		s.Logf("vareport: unmarshal: %v", err)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
		return
	}
	if req.Type != "va_report" {
		s.Logf("vareport: bad type %q", req.Type)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "expected va_report"})
		return
	}
	if len(fds) != 1 {
		s.Logf("vareport: expected 1 fd via SCM_RIGHTS, got %d", len(fds))
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "expected exactly one fd via SCM_RIGHTS"})
		return
	}

	s.acceptedMu.Lock()
	if s.accepted {
		s.acceptedMu.Unlock()
		s.Logf("vareport: duplicate report (zone=%s)", req.ZoneID)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "duplicate va_report"})
		return
	}
	if err := s.AddrMap.RegisterVMA(ProcessCH, req.VAStart, req.Size); err != nil {
		s.acceptedMu.Unlock()
		s.Logf("vareport: register vma failed: %v", err)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
		return
	}
	uffdFD := fds[0]
	if s.OnReady != nil {
		if err := s.OnReady(uffdFD, req.VAStart, req.Size); err != nil {
			s.acceptedMu.Unlock()
			s.Logf("vareport: OnReady failed: %v", err)
			_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
			return
		}
		// OnReady took ownership of uffdFD; remove from fds slice so
		// the deferred closeAll doesn't double-close.
		fds[0] = -1
	}
	s.accepted = true
	s.acceptedMu.Unlock()

	s.Logf("vareport: accepted zone=%s va=0x%x size=%d uffd_fd=%d",
		req.ZoneID, req.VAStart, req.Size, uffdFD)
	if err := writeLPJSON(conn, ackMsg{Type: "ack"}); err != nil {
		s.Logf("vareport: write ack: %v", err)
	}
}

// readLPJSONWithFDs reads u32 LE length + JSON body, possibly with
// SCM_RIGHTS-attached fds on the same recvmsg.
func readLPJSONWithFDs(c *net.UnixConn) ([]byte, []int, error) {
	// Read length prefix first (no FDs expected on this part).
	var lenBuf [4]byte
	oob := make([]byte, syscall.CmsgSpace(4*16)) // up to 16 fds
	n, oobN, _, _, err := c.ReadMsgUnix(lenBuf[:], oob)
	if err != nil {
		return nil, nil, fmt.Errorf("read length+oob: %w", err)
	}
	if n != 4 {
		return nil, nil, fmt.Errorf("short length read: %d", n)
	}
	fds := parseFDsFromOOB(oob[:oobN])

	bodyLen := binary.LittleEndian.Uint32(lenBuf[:])
	if bodyLen > 64*1024 {
		return nil, fds, fmt.Errorf("oversized message: %d bytes", bodyLen)
	}
	body := make([]byte, bodyLen)
	off := 0
	// In case fds came on the same recvmsg as the length but body is
	// in separate recvmsg(s), read the body now (no more OOB expected).
	for off < int(bodyLen) {
		nn, err := c.Read(body[off:])
		if err != nil {
			return body[:off], fds, fmt.Errorf("read body: %w", err)
		}
		off += nn
	}
	return body, fds, nil
}

func parseFDsFromOOB(oob []byte) []int {
	if len(oob) == 0 {
		return nil
	}
	cmsgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var out []int
	for _, c := range cmsgs {
		if c.Header.Level == syscall.SOL_SOCKET && c.Header.Type == syscall.SCM_RIGHTS {
			fds, err := syscall.ParseUnixRights(&c)
			if err == nil {
				out = append(out, fds...)
			}
		}
	}
	// Set CLOEXEC on each so a stray exec doesn't leak the uffd.
	for _, fd := range out {
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	}
	return out
}

func closeAll(fds []int) {
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}

func writeLPJSON(w net.Conn, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
