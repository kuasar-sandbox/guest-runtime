package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/mux"
)

// muxPair wires a guest-side and host-side mux.Session over a net.Pipe so
// the console-bridge pumps can be exercised without a real vsock.
func muxPair(t *testing.T, ss mux.StreamSet) (guest, host *mux.Session) {
	t.Helper()
	g, h := net.Pipe()
	guest = mux.NewSession(g, ss, mux.Options{})
	host = mux.NewSession(h, ss, mux.Options{})
	t.Cleanup(func() { _ = guest.Close(); _ = host.Close() })
	return guest, host
}

func TestPumpAppToHost(t *testing.T) {
	ss := mux.PipeStreams(false, true, false) // stdout only
	guest, host := muxPair(t, ss)

	holder := newSessionHolder()
	holder.set(guest)
	pr, pw := io.Pipe() // pr = sandbox-init's read end of the app's stdout
	pumped := make(chan struct{})
	go func() { pumpAppToHost(pr, holder, mux.StreamStdout); close(pumped) }()

	want := []byte("hello from the app\n")
	go func() { _, _ = pw.Write(want); _ = pw.Close() }() // app writes, then exits

	got, err := io.ReadAll(host.Stream(mux.StreamStdout))
	if err != nil {
		t.Fatalf("read host StreamStdout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("host got %q, want %q", got, want)
	}
	select {
	case <-pumped:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpAppToHost did not exit after app EOF")
	}
}

func TestPumpHostToApp(t *testing.T) {
	ss := mux.PipeStreams(true, false, false) // stdin only
	guest, host := muxPair(t, ss)

	holder := newSessionHolder()
	holder.set(guest)
	pr, pw := io.Pipe() // pw = sandbox-init writes the app's stdin; pr = app reads
	go pumpHostToApp(pw, holder, mux.StreamStdin, true)

	want := []byte("keyboard input\n")
	go func() {
		st := host.Stream(mux.StreamStdin)
		_, _ = st.Write(want)
		_ = st.CloseWrite() // host closed its stdin → app's stdin should hit EOF
	}()

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("app read stdin: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("app got %q, want %q", got, want)
	}
}

func TestSessionHolderReattach(t *testing.T) {
	holder := newSessionHolder()

	// Pump starts with no session — it parks. The app writes anyway; the
	// pump reads the bytes and then blocks in holder.wait() holding them.
	pr, pw := io.Pipe()
	pumped := make(chan struct{})
	go func() { pumpAppToHost(pr, holder, mux.StreamStdout); close(pumped) }()
	go func() { _, _ = pw.Write([]byte("buffered while detached\n")); _ = pw.Close() }()

	// Attach a session — the parked pump must flush the held bytes + EOF.
	ss := mux.PipeStreams(false, true, false)
	guest, host := muxPair(t, ss)
	holder.set(guest)

	got, err := io.ReadAll(host.Stream(mux.StreamStdout))
	if err != nil {
		t.Fatalf("read after reattach: %v", err)
	}
	if string(got) != "buffered while detached\n" {
		t.Fatalf("got %q, want %q", got, "buffered while detached\n")
	}
	select {
	case <-pumped:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after reattach + EOF")
	}
}

func TestSessionHolderShutdownWakesWaiters(t *testing.T) {
	holder := newSessionHolder()
	got := make(chan *mux.Session, 1)
	go func() { got <- holder.wait() }()
	time.Sleep(20 * time.Millisecond) // let the goroutine park in cond.Wait

	holder.shutdown()
	select {
	case s := <-got:
		if s != nil {
			t.Fatalf("wait() after shutdown returned %v, want nil", s)
		}
	case <-time.After(time.Second):
		t.Fatal("wait() did not unblock on shutdown")
	}
	if holder.peek() != nil {
		t.Fatal("peek after shutdown should be nil")
	}
}
