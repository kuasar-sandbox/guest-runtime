package mux

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type brokenFrameConn struct {
	mu                sync.Mutex
	wire              bytes.Buffer
	remaining, writes int
	failure           error
	closed            chan struct{}
	once              sync.Once
}

func (c *brokenFrameConn) Read([]byte) (int, error) { <-c.closed; return 0, io.EOF }
func (c *brokenFrameConn) Close() error             { c.once.Do(func() { close(c.closed) }); return nil }
func (c *brokenFrameConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	n := min(len(b), c.remaining)
	c.wire.Write(b[:n])
	c.remaining -= n
	if n < len(b) {
		return n, c.failure
	}
	return n, nil
}

func TestFrameWriteFailureRetiresSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining int
		update    bool
	}{
		{"partial-header", 2, false}, {"partial-payload", FrameHeaderLen + 2, false},
		{"window-header", 2, true}, {"window-payload", FrameHeaderLen + 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := errors.New("injected transport failure")
			conn := &brokenFrameConn{remaining: tc.remaining, failure: failure, closed: make(chan struct{})}
			s := NewSession(conn, PipeStreams(true, true, true), Options{Window: 8})
			defer s.Close()
			credit := s.Stream(StreamStderr)
			credit.mu.Lock()
			credit.credit = 0
			credit.mu.Unlock()
			creditDone, readDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := credit.Write([]byte("waiting")); creditDone <- err }()
			go func() { _, err := s.Stream(StreamStdin).Read(make([]byte, 1)); readDone <- err }()
			st := s.Stream(StreamStdout)
			failed := make(chan error, 1)
			go func() {
				if tc.update {
					if err := st.deliver([]byte("read")); err != nil {
						failed <- err
						return
					}
					n, err := st.Read(make([]byte, 4))
					if n != 4 {
						failed <- errors.New("lost consumed bytes")
						return
					}
					failed <- err
				} else {
					_, err := st.Write([]byte("payload"))
					failed <- err
				}
			}()
			select {
			case err := <-failed:
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("frame failure deadlocked under writer barrier")
			}
			select {
			case <-s.Done():
			case <-time.After(time.Second):
				t.Fatal("read loop did not stop")
			}
			if !errors.Is(s.Err(), failure) {
				t.Fatal("first write error lost:", s.Err())
			}
			for _, done := range []chan error{creditDone, readDone} {
				select {
				case err := <-done:
					if !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("stream waiter not woken")
				}
			}
			conn.mu.Lock()
			written, calls := conn.wire.Len(), conn.writes
			conn.mu.Unlock()
			_ = st.CloseWrite()
			_ = st.Reset()
			_ = s.SendExitStatus(0)
			_ = s.SetWinsize(80, 24)
			_ = s.InitMuxClose()
			if err := s.InitMuxClose(); !errors.Is(err, failure) {
				t.Fatal("repeated graceful close lost error:", err)
			}
			for i := 0; i < 3; i++ {
				_ = s.Close()
			}
			conn.mu.Lock()
			defer conn.mu.Unlock()
			if conn.writes != calls || conn.wire.Len() != written || written != tc.remaining {
				t.Fatal("appended bytes after incomplete frame")
			}
			if _, ok := s.ExitStatus(); ok {
				t.Fatal("failed transport manufactured exit status")
			}
		})
	}
}

func TestFailedGracefulCloseWakesAllCallers(t *testing.T) {
	failure := errors.New("close frame failed")
	c := &brokenFrameConn{remaining: 1, failure: failure, closed: make(chan struct{})}
	s := NewSession(c, PipeStreams(true, false, false), Options{})
	defer s.Close()
	done := make(chan error, 4)
	for i := 0; i < cap(done); i++ {
		go func() { done <- s.InitMuxClose() }()
	}
	for i := 0; i < cap(done); i++ {
		select {
		case err := <-done:
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("close waiter stuck")
		}
	}
}
