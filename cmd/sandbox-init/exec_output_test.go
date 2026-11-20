package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux"
)

func TestExecOutputDrainsCompleteArtifactAfterBackpressure(t *testing.T) {
	for _, window := range []int{4096, mux.DefaultWindow} {
		for _, delayReceive := range []bool{false, true} {
			t.Run(fmt.Sprintf("window-%d/delay-%t", window, delayReceive), func(t *testing.T) {
				payload := bytes.Repeat([]byte{0x5a}, window+9*1024)
				src, err := sparse.NewSource(bytes.NewReader(payload), uint64(len(payload)), nil)
				if err != nil {
					t.Fatal(err)
				}
				var artifact bytes.Buffer
				if _, _, err := tarstream.WriteTo(context.Background(), &artifact, "image", src); err != nil {
					t.Fatal(err)
				}
				fixture := artifact.Bytes()
				if _, _, err := tarstream.SourceAt(bytes.NewReader(fixture), int64(len(fixture)), "image"); err != nil {
					t.Fatalf("invalid fixture: %v", err)
				}
				f := newExecOutputFixture(t, fixture, window, false)
				done := f.finish()
				if delayReceive {
					// The old implementation returned after its fixed two-second
					// drain and reported exit 0 with only one output window.
					select {
					case err := <-done:
						t.Fatalf("exec completed before pending output could be consumed: %v", err)
					case <-time.After(2200 * time.Millisecond):
					}
				}
				got, err := io.ReadAll(f.host.Stream(mux.StreamStdout))
				if err != nil {
					t.Fatalf("read output: %v", err)
				}
				awaitExecFinish(t, done, false)
				if code, ok := f.host.ExitStatus(); !ok || code != 0 {
					t.Fatalf("host exit = (%d,%t), want (0,true)", code, ok)
				}
				if !bytes.Equal(got, fixture) {
					t.Fatalf("output bytes = %d, want %d", len(got), len(fixture))
				}
				if _, _, err := tarstream.SourceAt(bytes.NewReader(got), int64(len(got)), "image"); err != nil {
					t.Fatalf("received artifact: %v", err)
				}
				t.Logf("producer_exit=0 host_exit=0 window=%d delayed=%t fixture=%d received=%d", window, delayReceive, len(fixture), len(got))
			})
		}
	}
}

func TestExecOutputInheritedWriterKeepsSessionUntilEOF(t *testing.T) {
	fixture := []byte("complete output from exited child")
	f := newExecOutputFixture(t, fixture, 4096, true)
	done := f.finish()
	got := make([]byte, len(fixture))
	if _, err := io.ReadFull(f.host.Stream(mux.StreamStdout), got); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("exec completed while another writer still held stdout: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.writer.Close(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(f.host.Stream(mux.StreamStdout)); err != nil || len(rest) != 0 {
		t.Fatalf("EOF after last writer close: bytes=%d err=%v", len(rest), err)
	}
	awaitExecFinish(t, done, false)
	if code, ok := f.host.ExitStatus(); !ok || code != 0 || !bytes.Equal(got, fixture) {
		t.Fatalf("exit=(%d,%t), output=%q", code, ok, got)
	}
}

func TestExecOutputCancellationJoinsBlockedPumpsWithoutExitStatus(t *testing.T) {
	for _, heldWriter := range []bool{false, true} {
		for _, closeGuest := range []bool{false, true} {
			t.Run(fmt.Sprintf("inherited-writer-%t/guest-close-%t", heldWriter, closeGuest), func(t *testing.T) {
				data := bytes.Repeat([]byte{0x59}, 12*1024)
				if heldWriter {
					data = []byte("child finished")
				}
				f := newExecOutputFixture(t, data, 4096, heldWriter)
				done := f.finish()
				// One case blocks on MUX credit, the other on pipe EOF. Closing
				// the raw guest connection models the registry's quiesce close;
				// closing the peer models client cancellation/disconnection.
				if closeGuest {
					_ = f.guestConn.Close()
				} else {
					_ = f.host.Close()
				}
				awaitExecFinish(t, done, true)
				if code, ok := f.host.ExitStatus(); ok {
					t.Fatalf("interrupted output reported exit status %d", code)
				}
			})
		}
	}
}

func TestExecOutputReadFailureDoesNotReportExitStatus(t *testing.T) {
	g, h := net.Pipe()
	streams := mux.PipeStreams(true, true, true)
	guest := mux.NewSession(g, streams, mux.Options{})
	host := mux.NewSession(h, streams, mux.Options{})
	defer host.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	defer w.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderrW.Close()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinR.Close()
	// Failure of stdout must also unblock the stderr reader and stdin MUX
	// reader before finishExecSession returns, even with their peers open.
	bridge := &execBridge{stdoutR: r, stderrR: stderrR, stdinW: stdinW}
	bridge.pump(guest)
	done := make(chan error, 1)
	go func() { done <- finishExecSession(guest, bridge, 0) }()
	awaitExecFinish(t, done, true)
	if code, ok := host.ExitStatus(); ok {
		t.Fatalf("failed output reported exit status %d", code)
	}
}

func TestExecOutputWaitsForBothStreamsAndPreservesChildStatus(t *testing.T) {
	g, h := net.Pipe()
	streams := mux.PipeStreams(false, true, true)
	guest := mux.NewSession(g, streams, mux.Options{})
	host := mux.NewSession(h, streams, mux.Options{})
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	bridge := &execBridge{stdoutR: stdoutR, stderrR: stderrR}
	bridge.pump(guest)
	t.Cleanup(func() { _ = stdoutW.Close(); _ = stderrW.Close(); _ = host.Close(); bridge.abort(guest) })
	if _, err := stdoutW.Write([]byte("stdout complete")); err != nil {
		t.Fatal(err)
	}
	_ = stdoutW.Close()
	done := make(chan error, 1)
	go func() { done <- finishExecSession(guest, bridge, 17) }()
	if data, err := io.ReadAll(host.Stream(mux.StreamStdout)); err != nil || string(data) != "stdout complete" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
	select {
	case err := <-done:
		t.Fatalf("exec completed before stderr EOF: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := stderrW.Write([]byte("stderr tail")); err != nil {
		t.Fatal(err)
	}
	_ = stderrW.Close()
	if data, err := io.ReadAll(host.Stream(mux.StreamStderr)); err != nil || string(data) != "stderr tail" {
		t.Fatalf("stderr=%q err=%v", data, err)
	}
	awaitExecFinish(t, done, false)
	if code, ok := host.ExitStatus(); !ok || code != 17 {
		t.Fatalf("exit=(%d,%t), want (17,true)", code, ok)
	}
}

func TestExecOutputPTYReportsNaturalEOF(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	g, h := net.Pipe()
	streams := mux.StreamSet{mux.StreamPTY: true}
	guest := mux.NewSession(g, streams, mux.Options{})
	host := mux.NewSession(h, streams, mux.Options{})
	bridge := &execBridge{tty: true, ptyMaster: master}
	bridge.pump(guest)
	t.Cleanup(func() { _ = slave.Close(); _ = host.Close(); bridge.abort(guest) })
	data := []byte("pty-tail")
	if _, err := slave.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	done := make(chan error, 1)
	go func() { done <- finishExecSession(guest, bridge, 0) }()
	got, err := io.ReadAll(host.Stream(mux.StreamPTY))
	if err != nil {
		t.Fatal(err)
	}
	awaitExecFinish(t, done, false)
	if code, ok := host.ExitStatus(); !ok || code != 0 || !bytes.Equal(got, data) {
		t.Fatalf("exit=(%d,%t), output=%q", code, ok, got)
	}
}

type execOutputFixture struct {
	bridge    *execBridge
	guest     *mux.Session
	host      *mux.Session
	guestConn net.Conn
	writer    *os.File
}

func (f *execOutputFixture) finish() <-chan error {
	done := make(chan error, 1)
	go func() { done <- finishExecSession(f.guest, f.bridge, 0) }()
	return done
}

func newExecOutputFixture(t *testing.T, data []byte, window int, keepWriter bool) *execOutputFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "producer-output")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	g, h := net.Pipe()
	observed := &execOutputObservedConn{Conn: g, threshold: min(window, len(data)) + mux.FrameHeaderLen, reached: make(chan struct{})}
	streams := mux.PipeStreams(false, true, false)
	guest := mux.NewSession(observed, streams, mux.Options{Window: window})
	host := mux.NewSession(h, streams, mux.Options{Window: window})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	f := &execOutputFixture{bridge: &execBridge{stdoutR: r}, guest: guest, host: host, guestConn: g, writer: w}
	f.bridge.pump(guest)
	t.Cleanup(func() { _ = host.Close(); f.bridge.abort(guest); _ = w.Close() })
	producer := exec.Command("cat", path)
	producer.Stdout = w
	if err := producer.Start(); err != nil {
		t.Fatal(err)
	}
	if !keepWriter {
		_ = w.Close()
	}
	done := make(chan error, 1)
	go func() { done <- producer.Wait() }()
	select {
	case err := <-done:
		if err != nil || !producer.ProcessState.Success() {
			t.Fatalf("producer did not successfully exit before drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = producer.Process.Kill()
		<-done
		t.Fatal("producer did not fit pipe/pump staging capacity")
	}
	select {
	case <-observed.reached:
	case <-time.After(time.Second):
		t.Fatal("initial output window did not arrive")
	}
	return f
}

func awaitExecFinish(t *testing.T, done <-chan error, wantError bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err != nil) != wantError {
			t.Fatalf("finish error = %v, wantError=%t", err, wantError)
		}
	case <-time.After(time.Second):
		t.Fatal("exec completion did not join its pumps promptly")
	}
}

type execOutputObservedConn struct {
	net.Conn
	written   atomic.Int64
	threshold int
	reached   chan struct{}
	once      sync.Once
}

func (c *execOutputObservedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.written.Add(int64(n)) >= int64(c.threshold) {
		c.once.Do(func() { close(c.reached) })
	}
	return n, err
}
