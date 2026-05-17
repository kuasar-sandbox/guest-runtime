package mux

import (
	"io"
	"sync"
)

// Stream is one logical data stream on a MUX connection (StreamStdin /
// StreamStdout / StreamStderr / StreamPTY). It is symmetric:
//
//   - Read drains bytes the peer sent on this stream (and sends
//     WINDOW_UPDATE back as it consumes, so the peer can keep sending).
//   - Write frames bytes to the peer on this stream, blocking until the
//     peer has granted enough send credit (flow control / backpressure).
//   - CloseWrite sends EOF (half-close of our write direction).
//   - Reset aborts the stream in both directions.
//
// In pipe mode each stream is used in one direction only (host writes
// StreamStdin / reads StreamStdout&StreamStderr; guest the reverse). In
// tty mode StreamPTY is used in both directions on both sides.
type Stream struct {
	s      *Session
	id     uint8
	window int

	mu   sync.Mutex
	cond *sync.Cond

	// receive side
	buf      []byte
	rEOF     bool // peer sent EOF
	rReset   bool // peer sent RESET (or we Reset()'d)
	consumed int  // bytes Read since the last WINDOW_UPDATE we sent

	// send side
	credit  int  // remaining send credit (bytes the peer will accept)
	wClosed bool // we sent EOF
	wReset  bool // RESET in effect (no more sends)

	dead bool // the session ended
}

func newStream(s *Session, id uint8, window int) *Stream {
	st := &Stream{s: s, id: id, window: window, credit: window}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// ID returns the stream id.
func (st *Stream) ID() uint8 { return st.id }

// --- delivery hooks called from Session.readLoop --------------------

func (st *Stream) deliver(p []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rEOF || st.rReset {
		return ErrProtocol // DATA after EOF / RESET
	}
	if len(st.buf)+len(p) > st.window {
		return ErrProtocol // peer exceeded the receive window
	}
	st.buf = append(st.buf, p...)
	st.cond.Broadcast()
	return nil
}

func (st *Stream) deliverEOF() {
	st.mu.Lock()
	st.rEOF = true
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) deliverReset() {
	st.mu.Lock()
	st.rReset = true
	st.wReset = true
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) addCredit(delta int) {
	st.mu.Lock()
	st.credit += delta
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) shutdown() {
	st.mu.Lock()
	st.dead = true
	st.cond.Broadcast()
	st.mu.Unlock()
}

// --- io.Reader ------------------------------------------------------

// Read drains buffered bytes; blocks until data is available or the
// peer half-closed (io.EOF) / reset (ErrStreamReset) / the session
// ended (ErrClosed). As it consumes, it tops the peer's send credit
// back up with a WINDOW_UPDATE.
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for len(st.buf) == 0 {
		switch {
		case st.rReset:
			st.mu.Unlock()
			return 0, ErrStreamReset
		case st.rEOF:
			st.mu.Unlock()
			return 0, io.EOF
		case st.dead:
			st.mu.Unlock()
			return 0, ErrClosed
		}
		st.cond.Wait()
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	if len(st.buf) == 0 {
		st.buf = nil
	}
	st.consumed += n
	update := 0
	if st.consumed >= st.window/2 {
		update = st.consumed
		st.consumed = 0
	}
	st.mu.Unlock()
	if update > 0 {
		// Best-effort: if the conn is dying, the read loop surfaces it.
		_ = st.s.writeFrame(Frame{Stream: st.id, Type: FrameWindowUpdate, Payload: encodeUint32(uint32(update))})
	}
	return n, nil
}

// --- io.Writer ------------------------------------------------------

// acquireUpTo blocks until at least 1 byte of send credit is available,
// then deducts and returns min(want, available). Always makes progress
// regardless of how the peer's window compares to want.
func (st *Stream) acquireUpTo(want int) (int, error) {
	st.mu.Lock()
	for st.credit <= 0 {
		switch {
		case st.wReset:
			st.mu.Unlock()
			return 0, ErrStreamReset
		case st.wClosed:
			st.mu.Unlock()
			return 0, ErrClosed
		case st.dead:
			st.mu.Unlock()
			return 0, ErrClosed
		}
		if st.s.refuseWrites() {
			st.mu.Unlock()
			return 0, ErrClosed // MUX_CLOSE initiated — no more data frames
		}
		st.cond.Wait()
	}
	n := st.credit
	if n > want {
		n = want
	}
	st.credit -= n
	st.mu.Unlock()
	return n, nil
}

// Write frames b as DATA on this stream, splitting into DataChunk-sized
// frames and blocking on flow control. Returns bytes written before any
// error.
func (st *Stream) Write(b []byte) (int, error) {
	st.mu.Lock()
	if st.wClosed || st.wReset || st.dead {
		st.mu.Unlock()
		return 0, ErrClosed
	}
	st.mu.Unlock()
	if st.s.refuseWrites() {
		return 0, ErrClosed
	}
	return st.s.writeData(st, b)
}

// CloseWrite sends EOF on this stream (idempotent). The peer's Read
// returns io.EOF once its buffer drains.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.wClosed || st.wReset {
		st.mu.Unlock()
		return nil
	}
	st.wClosed = true
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.s.writeFrame(Frame{Stream: st.id, Type: FrameEOF})
}

// Reset aborts this stream in both directions.
func (st *Stream) Reset() error {
	st.mu.Lock()
	if st.wReset {
		st.mu.Unlock()
		return nil
	}
	st.wReset = true
	st.rReset = true
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.s.writeFrame(Frame{Stream: st.id, Type: FrameReset})
}
