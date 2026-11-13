package main

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// delayedOutputConn models a buffered transport: a successful guest Write
// only queues bytes locally, not delivery to the host MUX reader. A plain
// net.Pipe cannot expose this distinction because each Write waits for Read.
type delayedOutputConn struct {
	net.Conn
	mu          sync.Mutex
	pending     bytes.Buffer
	released    bool
	closeQueued chan struct{}
	closeOnce   sync.Once
}

func (c *delayedOutputConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return c.Conn.Write(p)
	}
	n, err := c.pending.Write(p)
	frames := bytes.NewReader(c.pending.Bytes())
	for {
		frame, frameErr := mux.ReadFrame(frames)
		if frameErr != nil {
			break
		}
		if frame.Type == mux.FrameMuxClose {
			c.closeOnce.Do(func() { close(c.closeQueued) })
		}
	}
	return n, err
}

func (c *delayedOutputConn) release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = true
	_, err := io.Copy(c.Conn, &c.pending)
	return err
}

func TestAppExitedWaitsForHostOutputBarrier(t *testing.T) {
	g, h := net.Pipe()
	transport := &delayedOutputConn{Conn: g, closeQueued: make(chan struct{})}
	ss := mux.PipeStreams(false, true, true)
	guest := mux.NewSession(transport, ss, mux.Options{})
	host := mux.NewSession(h, ss, mux.Options{})
	t.Cleanup(func() { _ = guest.Close(); _ = host.Close() })

	cs, bridge, err := setupAppStdio(proto.StdioSpec{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeChildStdio(cs) })
	bridge.attach(guest)
	bridge.start()
	wantOut, wantErr := "PYBOOT-OK 312\n", "final stderr\n"
	if _, err := io.WriteString(cs.stdout, wantOut); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(cs.stderr, wantErr); err != nil {
		t.Fatal(err)
	}
	closeChildStdio(cs)

	exited := make(chan struct{})
	go func() { bridge.appExited(); close(exited) }()
	select {
	case <-exited:
		t.Fatal("appExited allowed power-off while stdout/stderr were only queued in the transport")
	case <-transport.closeQueued:
	case <-time.After(time.Second):
		t.Fatal("appExited did not request the host output barrier")
	}
	select {
	case <-exited:
		t.Fatal("appExited returned before the host acknowledged output")
	default:
	}
	if err := transport.release(); err != nil {
		t.Fatalf("deliver queued output: %v", err)
	}
	for _, stream := range []struct {
		id   uint8
		want string
	}{{mux.StreamStdout, wantOut}, {mux.StreamStderr, wantErr}} {
		got, err := io.ReadAll(host.Stream(stream.id))
		if err != nil || string(got) != stream.want {
			t.Fatalf("stream %d = %q, %v; want %q", stream.id, got, err, stream.want)
		}
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("appExited did not finish after host acknowledgement")
	}
}

func TestAppExitedBoundsMissingHostAcknowledgement(t *testing.T) {
	g, h := net.Pipe()
	transport := &delayedOutputConn{Conn: g, closeQueued: make(chan struct{})}
	guest := mux.NewSession(transport, mux.PipeStreams(false, true, false), mux.Options{})
	t.Cleanup(func() { _ = guest.Close(); _ = h.Close() })
	cs, bridge, err := setupAppStdio(proto.StdioSpec{Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	bridge.attach(guest)
	bridge.start()
	closeChildStdio(cs)
	exited := make(chan struct{})
	go func() { bridge.appExitedWithTimeout(50 * time.Millisecond); close(exited) }()
	select {
	case <-transport.closeQueued:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not request MUX acknowledgement")
	}
	// Never release the transport or send an ACK. Guest shutdown must remain
	// bounded, and its hard close must retire the blocked handshake goroutine.
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown hung waiting for host acknowledgement")
	}
	select {
	case <-guest.Done():
	case <-time.After(time.Second):
		t.Fatal("timed-out MUX session was not closed")
	}
}

func TestAppExitedBoundsDetachedOutput(t *testing.T) {
	cs, bridge, err := setupAppStdio(proto.StdioSpec{Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	bridge.start() // no attached host: pump must wait with its final bytes
	if _, err := io.WriteString(cs.stdout, "detached output\n"); err != nil {
		t.Fatal(err)
	}
	closeChildStdio(cs)
	exited := make(chan struct{})
	go func() { bridge.appExitedWithTimeout(50 * time.Millisecond); close(exited) }()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown hung waiting for a detached host")
	}
	pumpsDone := make(chan struct{})
	go func() { bridge.wg.Wait(); close(pumpsDone) }()
	select {
	case <-pumpsDone:
	case <-time.After(time.Second):
		t.Fatal("detached output pump leaked after shutdown")
	}
}

type lingeringCloseConn struct {
	net.Conn
	closeStarted chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func (c *lingeringCloseConn) Close() error {
	c.closeOnce.Do(func() { close(c.closeStarted) })
	<-c.releaseClose
	return c.Conn.Close()
}

func TestAppExitedBoundsTransportLinger(t *testing.T) {
	g, h := net.Pipe()
	transport := &lingeringCloseConn{Conn: g, closeStarted: make(chan struct{}), releaseClose: make(chan struct{})}
	ss := mux.PipeStreams(false, true, false)
	guest := mux.NewSession(transport, ss, mux.Options{})
	host := mux.NewSession(h, ss, mux.Options{})
	t.Cleanup(func() {
		close(transport.releaseClose)
		_ = guest.Close()
		_ = host.Close()
	})
	cs, bridge, err := setupAppStdio(proto.StdioSpec{Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	bridge.attach(guest)
	bridge.start()
	closeChildStdio(cs)
	exited := make(chan struct{})
	go func() { bridge.appExitedWithTimeout(50 * time.Millisecond); close(exited) }()
	select {
	case <-transport.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not close the acknowledged transport")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("transport linger extended the shutdown deadline")
	}
}

func TestAppExitedBoundsHostEOFWithoutAck(t *testing.T) {
	g, h := net.Pipe()
	guest := mux.NewSession(g, mux.PipeStreams(false, true, false), mux.Options{})
	t.Cleanup(func() { _ = guest.Close(); _ = h.Close() })
	cs, bridge, err := setupAppStdio(proto.StdioSpec{Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	bridge.attach(guest)
	bridge.start()
	closeChildStdio(cs)
	peerDone := make(chan error, 1)
	go func() {
		defer h.Close()
		for {
			frame, err := mux.ReadFrame(h)
			if err != nil || frame.Type == mux.FrameMuxClose {
				peerDone <- err
				return // deliberately close without sending MUX_CLOSE_ACK
			}
		}
	}()
	exited := make(chan struct{})
	go func() { bridge.appExited(); close(exited) }()
	select {
	case err := <-peerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not receive MUX_CLOSE")
	}
	// EOF without ACK is a transport failure, not a delivery guarantee. It
	// remains a terminal best-effort shutdown, rather than adding a new delay.
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after host EOF without acknowledgement")
	}
}
