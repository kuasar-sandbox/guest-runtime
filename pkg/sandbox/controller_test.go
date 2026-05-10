package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCHAPIServer mimics CH's HTTP/1.1 unix-socket API for testing
// chAPISendWithTiming. Each accepted connection invokes onConn (which
// can stall, send a partial response, return early, etc.).
type fakeCHAPIServer struct {
	sock     string
	listener *net.UnixListener
	wg       sync.WaitGroup
	stopped  bool
	mu       sync.Mutex
}

func newFakeCHAPIServer(t *testing.T, onConn func(c *net.UnixConn)) *fakeCHAPIServer {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "ch.sock")
	addr, _ := net.ResolveUnixAddr("unix", sock)
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeCHAPIServer{sock: sock, listener: l}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func(c *net.UnixConn) {
				defer s.wg.Done()
				defer c.Close()
				onConn(c)
			}(c)
		}
	}()
	return s
}

func (s *fakeCHAPIServer) close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	_ = s.listener.Close()
	s.wg.Wait()
}

// TestChAPISend_Happy_204: normal CH /vm.resize response — 204 No
// Content. Verifies all timing stages are populated and increasing.
func TestChAPISend_Happy_204(t *testing.T) {
	srv := newFakeCHAPIServer(t, func(c *net.UnixConn) {
		_, _ = io.ReadAll(io.LimitReader(c, 4096))
		_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nServer: fake\r\nConnection: close\r\n\r\n"))
	})
	defer srv.close()
	// Drain client request: fakeCHAPIServer's onConn has io.ReadAll on
	// LimitReader that may not return until client closes write side.
	// chAPISendWithTiming uses Connection: close so client write side
	// stays open — instead re-implement onConn to read header+body
	// via a simple Read call. Above is fine because chAPISendWithTiming
	// closes after write completes? Actually no: HTTP/1.1 client
	// keeps conn open until server responds. Use a smarter onConn.
	srv.close()

	srv = newFakeCHAPIServer(t, func(c *net.UnixConn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf) // one read is enough — request fits
		_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nServer: fake\r\nConnection: close\r\n\r\n"))
	})
	defer srv.close()

	timing, err := chAPISendWithTiming(srv.sock, "PUT", "/api/v1/vm.resize", `{"desired_balloon":1024}`)
	if err != nil {
		t.Fatalf("expected success, got: %v (timing=%s)", err, timing)
	}
	if timing.Dial == 0 {
		t.Errorf("Dial timing not recorded: %s", timing)
	}
	if timing.Write < timing.Dial {
		t.Errorf("Write %v should be >= Dial %v", timing.Write, timing.Dial)
	}
	if timing.FirstByte == 0 {
		t.Errorf("FirstByte timing not recorded: %s", timing)
	}
	if timing.FirstByte < timing.Write {
		t.Errorf("FirstByte %v should be >= Write %v", timing.FirstByte, timing.Write)
	}
	if timing.Total < timing.FirstByte {
		t.Errorf("Total %v should be >= FirstByte %v", timing.Total, timing.FirstByte)
	}
	t.Logf("happy path timing: %s", timing)
}

// TestChAPISend_NonTerminating_HitsDeadline: server accepts the
// connection but never writes a response. Verifies the read deadline
// fires and the error is surfaced (not silently swallowed as an empty
// response). This is the regression test for the previous code's
// `n, _ := c.Read(buf)` which would have returned a "short response"
// error with no indication that a timeout occurred.
func TestChAPISend_NonTerminating_HitsDeadline(t *testing.T) {
	// Override the 10s deadline locally — this is hardcoded in the
	// production path, but we can rely on the test running at most a
	// few seconds. Use a quick block: server reads request then sleeps
	// past the 10s deadline.
	if testing.Short() {
		t.Skip("skipping 10s+ deadline test in short mode")
	}
	srv := newFakeCHAPIServer(t, func(c *net.UnixConn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		// Sleep long past the production 10s deadline.
		time.Sleep(15 * time.Second)
	})
	defer srv.close()

	t0 := time.Now()
	timing, err := chAPISendWithTiming(srv.sock, "PUT", "/api/v1/vm.resize", `{"desired_balloon":0}`)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	t.Logf("got expected error: %v (timing=%s, total elapsed=%v)", err, timing, elapsed)

	// Must surface the timeout — not just "short response".
	msg := err.Error()
	if !strings.Contains(msg, "read failed") && !strings.Contains(msg, "i/o timeout") &&
		!strings.Contains(msg, "deadline") {
		t.Errorf("error should surface read failure, got: %v", err)
	}
	// Total must reflect that we waited the full deadline (~10s).
	if timing.Total < 9*time.Second {
		t.Errorf("expected total ~10s, got %s", timing.Total)
	}
	if timing.Total > 12*time.Second {
		t.Errorf("total took too long: %s", timing.Total)
	}
}

// TestChAPISend_PartialResponse_DrainedAfterEOF: server sends a valid
// 204 response but split across two writes with a small delay. Verify
// the loop accumulates bytes correctly (regression for `n, _ := c.Read`
// which only got the first segment).
func TestChAPISend_PartialResponse_DrainedAfterEOF(t *testing.T) {
	srv := newFakeCHAPIServer(t, func(c *net.UnixConn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.1 "))
		time.Sleep(20 * time.Millisecond)
		_, _ = c.Write([]byte("204 No Content\r\nConnection: close\r\n\r\n"))
	})
	defer srv.close()

	timing, err := chAPISendWithTiming(srv.sock, "PUT", "/api/v1/vm.resize", `{"desired_balloon":4096}`)
	if err != nil {
		t.Fatalf("expected success despite split response, got: %v (timing=%s)", err, timing)
	}
	if timing.FirstByte > timing.Total {
		t.Errorf("invariant violation: %s", timing)
	}
}

// TestChAPISend_ConnRefused: socket doesn't exist → dial fails → error
// surfaces immediately and timing.Total reflects only dial.
func TestChAPISend_ConnRefused(t *testing.T) {
	timing, err := chAPISendWithTiming("/nonexistent/path.sock", "GET", "/api/v1/vm.info", "")
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Errorf("expected dial error, got: %v", err)
	}
	if timing.Total > 100*time.Millisecond {
		t.Errorf("dial-fail returned too slowly: %s", timing)
	}
	if timing.Write != 0 {
		t.Errorf("Write should be 0 when dial fails: %s", timing)
	}
}

// TestChAPISend_Non2xx: 400 response surfaces error with full status
// and timing.
func TestChAPISend_Non2xx(t *testing.T) {
	srv := newFakeCHAPIServer(t, func(c *net.UnixConn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\nbad json"))
	})
	defer srv.close()

	timing, err := chAPISendWithTiming(srv.sock, "PUT", "/api/v1/vm.resize", `{}`)
	if err == nil {
		t.Fatal("expected non-2xx error, got nil")
	}
	if !strings.Contains(err.Error(), "non-2xx") {
		t.Errorf("expected non-2xx in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "timing") {
		t.Errorf("error should embed timing for diagnostics, got: %v", err)
	}
	t.Logf("non-2xx error: %v (timing %s)", err, timing)
}

// TestChAPITiming_String: format check.
func TestChAPITiming_String(t *testing.T) {
	tm := chAPITiming{
		Dial:      150 * time.Microsecond,
		Write:     200 * time.Microsecond,
		FirstByte: 350 * time.Microsecond,
		Total:     400 * time.Microsecond,
	}
	s := tm.String()
	for _, want := range []string{"dial=", "write=", "first_byte=", "total="} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

// silence unused
var _ = errors.New
var _ = fmt.Sprintf
