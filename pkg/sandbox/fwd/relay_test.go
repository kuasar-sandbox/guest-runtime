package fwd

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// socketPair returns the two connected ends of an AF_UNIX SOCK_STREAM
// socketpair as *net.UnixConn (which, unlike net.Pipe, supports
// CloseWrite — required to exercise half-close).
func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return fileConn(t, fds[0]), fileConn(t, fds[1])
}

func fileConn(t *testing.T, fd int) net.Conn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "sp")
	c, err := net.FileConn(f)
	_ = f.Close() // FileConn dups the fd
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	return c
}

func readAllDeadline(t *testing.T, c net.Conn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// TestRelayHalfClose wires two relays back to back over a "vsock"
// socketpair — exactly the host↔guest topology — and verifies TCP
// half-close survives the round trip in BOTH directions:
//
//	client → [hostRelay] → vsock → [guestRelay] → target
//
// The client sends then half-closes; the target must see the data then a
// clean EOF (not a full reset) while still being able to reply; the reply
// + the target's half-close must likewise surface as EOF at the client.
func TestRelayHalfClose(t *testing.T) {
	hostV, guestV := socketPair(t)  // the vsock link (framed side of each relay)
	client, hostP := socketPair(t)  // host-local connection
	guestP, target := socketPair(t) // guest-side dial target

	host := NewRelay(hostV, hostP)
	guest := NewRelay(guestV, guestP)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); host.Run() }()
	go func() { defer wg.Done(); guest.Run() }()

	// client → target, then client half-closes its write side.
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := client.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	if got := readAllDeadline(t, target); !bytes.Equal(got, []byte("ping")) {
		t.Fatalf("target read: got %q want %q", got, "ping")
	}

	// target replies (the reverse direction must still be open after the
	// client's half-close), then half-closes.
	if _, err := target.Write([]byte("pong")); err != nil {
		t.Fatalf("target write: %v", err)
	}
	if err := target.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("target CloseWrite: %v", err)
	}
	if got := readAllDeadline(t, client); !bytes.Equal(got, []byte("pong")) {
		t.Fatalf("client read: got %q want %q", got, "pong")
	}

	waitTimeout(t, &wg, 3*time.Second) // both relays tear down cleanly
}

// TestRelayShutdown verifies external Shutdown unblocks an idle relay
// (the quiesce / run-teardown path) and that the spliced conn is closed.
func TestRelayShutdown(t *testing.T) {
	hostV, _ := socketPair(t)
	_, hostP := socketPair(t)
	r := NewRelay(hostV, hostP)
	done := make(chan struct{})
	go func() { r.Run(); close(done) }()

	// Nothing is flowing; Run is blocked on both reads. Shutdown must end it.
	var lingered bool
	r.Shutdown(func() { lingered = true })
	if !lingered {
		t.Error("Shutdown pre-hook not invoked")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
	// plain conn is closed → further writes fail.
	if _, err := hostP.Write([]byte("x")); err == nil {
		t.Error("expected write on closed plain conn to fail")
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("relays did not finish within deadline")
	}
}
