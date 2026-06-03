package fwd

import (
	"io"
	"net"
	"sync"
)

// Relay splices a framed reverse-channel connection (framed) to a plain
// byte-stream connection (plain), preserving TCP half-close. It is
// symmetric — the host constructs one with (vsock conn, local client
// conn) and the guest with (vsock conn, dial target conn); the framed
// side is always the vsock connection carrying the fwd sub-protocol.
//
//	plain → framed:  bytes → DATA frames; a clean read EOF → EOF frame
//	                 (half-close); a read error → RST frame
//	framed → plain:  DATA → plain.Write; EOF → plain.CloseWrite()
//	                 (the peer is done sending); RST / transport error → abort
//
// A clean shutdown is each direction independently reaching EOF; only
// once BOTH have are the connections fully closed, so a peer that
// half-closed its write side keeps receiving on the other direction.
// Any error path aborts both directions at once. Flow control is the
// framed connection's own kernel-buffer backpressure: a single logical
// stream per conn means no application window is needed.
//
// All teardown — clean completion, internal error, and the external
// Shutdown hook — funnels through one sync.Once, so the underlying conns
// are closed exactly once (important for the guest's raw-fd vsockConn,
// where a double close could hit a reused fd).
type Relay struct {
	framed io.ReadWriteCloser
	plain  net.Conn

	wmu  sync.Mutex // serializes frame writes to framed (both pumps may write)
	once sync.Once
}

// NewRelay builds a relay over an established framed conn and its spliced
// plain conn. Call Run to drive it.
func NewRelay(framed io.ReadWriteCloser, plain net.Conn) *Relay {
	return &Relay{framed: framed, plain: plain}
}

// Run drives both copy directions until each has ended (clean half-close)
// or the relay is aborted, then closes both connections. Blocks until the
// session is over.
func (r *Relay) Run() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.pumpOut() }()
	go func() { defer wg.Done(); r.pumpIn() }()
	wg.Wait()
	r.teardown(nil)
}

// Shutdown aborts the relay from outside (run teardown / snapshot quiesce),
// unblocking Run. pre, if non-nil, runs once just before the conns are
// closed — the guest passes a SO_LINGER arm so the vsock close is confirmed
// (no half-open remnant in a snapshot). Idempotent.
func (r *Relay) Shutdown(pre func()) { r.teardown(pre) }

func (r *Relay) teardown(pre func()) {
	r.once.Do(func() {
		if pre != nil {
			pre()
		}
		_ = r.framed.Close()
		_ = r.plain.Close()
	})
}

func (r *Relay) writeFrame(f Frame) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	return WriteFrame(r.framed, f)
}

// pumpOut copies plain → framed: bytes become DATA frames, a clean EOF
// becomes an EOF frame (half-close, leaving framed open for pumpIn), a
// read error becomes a RST frame + abort.
func (r *Relay) pumpOut() {
	buf := make([]byte, DataChunk)
	for {
		n, err := r.plain.Read(buf)
		if n > 0 {
			if werr := r.writeFrame(Frame{Type: FrameData, Payload: buf[:n]}); werr != nil {
				r.teardown(nil)
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				_ = r.writeFrame(Frame{Type: FrameEOF})
			} else {
				_ = r.writeFrame(Frame{Type: FrameRST})
				r.teardown(nil)
			}
			return
		}
	}
}

// pumpIn copies framed → plain: DATA frames become writes, an EOF frame
// half-closes plain's write side (the peer is done sending; pumpOut may
// still be feeding the other direction), a RST frame or any transport
// error aborts.
func (r *Relay) pumpIn() {
	for {
		f, err := ReadFrame(r.framed)
		if err != nil {
			r.teardown(nil)
			return
		}
		switch f.Type {
		case FrameData:
			if _, werr := r.plain.Write(f.Payload); werr != nil {
				_ = r.writeFrame(Frame{Type: FrameRST})
				r.teardown(nil)
				return
			}
		case FrameEOF:
			closeWrite(r.plain)
			return
		default: // FrameRST or unknown
			r.teardown(nil)
			return
		}
	}
}

// closeWrite half-closes c's write direction (shutdown SHUT_WR) so the
// peer reads EOF while the read direction stays open. *net.TCPConn and
// *net.UnixConn — the only conn types we splice — both implement it; the
// full-close fallback only triggers for an exotic conn with no half-close
// support.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
