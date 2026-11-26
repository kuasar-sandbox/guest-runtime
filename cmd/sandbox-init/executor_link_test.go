package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func executorPairForTest(t *testing.T) (*executorLink, *executorLink) {
	t.Helper()
	left, file, err := newExecutorPair()
	if err != nil {
		t.Fatal(err)
	}
	right, err := executorLinkFromFile(file)
	_ = file.Close()
	if err != nil {
		left.close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		left.close()
		right.close()
	})
	for _, link := range []*executorLink{left, right} {
		if err := link.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return left, right
}

func serveExecutorForTest(t *testing.T, link *executorLink, handler func(executorMessage, *int) (executorMessage, error)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		link.serve(handler)
	}()
	t.Cleanup(func() {
		link.close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("executor reader did not exit after close")
		}
	})
}

func noExecutorRequests(executorMessage, *int) (executorMessage, error) {
	return executorMessage{}, errors.New("unexpected request")
}

func TestExecutorLinkBidirectionalRequests(t *testing.T) {
	left, right := executorPairForTest(t)
	for _, link := range []*executorLink{left, right} {
		serveExecutorForTest(t, link, func(msg executorMessage, _ *int) (executorMessage, error) {
			switch msg.Op {
			case "echo":
				return executorMessage{PID: msg.PID}, nil
			case "callback":
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				reply, err := link.call(ctx, executorMessage{Op: "echo", PID: msg.PID}, -1)
				return executorMessage{PID: reply.PID, Code: reply.PID * 2}, err
			default:
				return executorMessage{}, fmt.Errorf("unexpected op %q", msg.Op)
			}
		})
	}
	const requests = 24
	start := make(chan struct{})
	results := make(chan error, requests*2)
	for i := 0; i < requests*2; i++ {
		link := []*executorLink{left, right}[i%2]
		go func(value int) {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			reply, err := link.call(ctx, executorMessage{Op: "callback", PID: value}, -1)
			if err == nil && (reply.PID != value || reply.Code != value*2) {
				err = fmt.Errorf("request %d received another request's reply: %+v", value, reply)
			}
			results <- err
		}(i + 1)
	}
	close(start)
	for i := 0; i < requests*2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("bidirectional requests deadlocked")
		}
	}
}

func TestExecutorLinkLargePayloads(t *testing.T) {
	value := strings.Repeat("launch-and-exec-environment=", 24_000)
	for _, tc := range []struct {
		name string
		msg  executorMessage
	}{
		{"launch", executorMessage{Op: "launch", Launch: &proto.LaunchSpec{Exec: "/bin/sh", Env: map[string]string{"LARGE": value}}}},
		{"exec", executorMessage{Op: "session", Request: &proto.Message{Type: proto.TypeExec, Exec: &proto.ExecSpec{Argv: []string{"/bin/sh"}, Env: map[string]string{"LARGE": value}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := executorPairForTest(t)
			for _, link := range []*executorLink{left, right} {
				if err := link.conn.SetWriteBuffer(4096); err != nil {
					t.Fatal(err)
				}
				if err := link.conn.SetReadBuffer(4096); err != nil {
					t.Fatal(err)
				}
			}
			serveExecutorForTest(t, left, noExecutorRequests)
			serveExecutorForTest(t, right, func(msg executorMessage, _ *int) (executorMessage, error) {
				return msg, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reply, err := left.call(ctx, tc.msg, -1)
			if err != nil {
				t.Fatal(err)
			}
			deadline, _ := ctx.Deadline()
			if reply.Deadline != deadline.UnixNano() {
				t.Fatal("RPC budget was not propagated")
			}
			reply.ID, reply.Reply, reply.Deadline = 0, false, 0
			if !reflect.DeepEqual(reply, tc.msg) {
				t.Fatal("payload changed across a stream frame larger than the socket buffers")
			}
		})
	}
}

func pipeEOFForExecutorTest(t *testing.T, file *os.File) {
	t.Helper()
	if err := file.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if n, err := file.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("descriptor remained open: read = %d, %v", n, err)
	}
}

func TestExecutorLinkDescriptorOwnership(t *testing.T) {
	for _, mode := range []string{"retained", "unclaimed", "handler-error"} {
		t.Run(mode, func(t *testing.T) {
			left, right := executorPairForTest(t)
			reader, sender, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer sender.Close()
			received := make(chan int, 1)
			serveExecutorForTest(t, left, noExecutorRequests)
			serveExecutorForTest(t, right, func(_ executorMessage, fd *int) (executorMessage, error) {
				flags, err := unix.FcntlInt(uintptr(*fd), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					return executorMessage{}, fmt.Errorf("received descriptor is not CLOEXEC: flags=%d, err=%v", flags, err)
				}
				received <- *fd
				if mode == "retained" {
					*fd = -1
				}
				if mode == "handler-error" {
					return executorMessage{}, errors.New("handler failed")
				}
				return executorMessage{}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = left.call(ctx, executorMessage{Op: "descriptor"}, int(sender.Fd()))
			if mode == "handler-error" {
				if err == nil || err.Error() != "handler failed" {
					t.Fatalf("handler error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var fd int
			select {
			case fd = <-received:
			case <-time.After(time.Second):
				t.Fatal("handler did not receive descriptor")
			}
			if _, err := unix.FcntlInt(sender.Fd(), unix.F_GETFD, 0); err != nil {
				t.Fatalf("send consumed the sender's descriptor: %v", err)
			}
			if err := sender.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "retained" {
				owned := os.NewFile(uintptr(fd), "retained-executor-descriptor")
				defer owned.Close()
				// Link closure must not close a descriptor already taken by its handler.
				left.close()
				right.close()
				if _, err := owned.WriteString("ok"); err != nil {
					t.Fatalf("retained descriptor was closed: %v", err)
				}
				if err := reader.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatal(err)
				}
				var b [2]byte
				if _, err := io.ReadFull(reader, b[:]); err != nil || string(b[:]) != "ok" {
					t.Fatalf("retained descriptor stream = %q, %v", b, err)
				}
				if err := owned.Close(); err != nil {
					t.Fatal(err)
				}
			}
			pipeEOFForExecutorTest(t, reader)
		})
	}
}

func TestExecutorLinkClosesMalformedFrameDescriptors(t *testing.T) {
	for _, mode := range []string{"zero-size", "too-large", "json", "partial-header", "partial-body", "extra-rights"} {
		t.Run(mode, func(t *testing.T) {
			left, right := executorPairForTest(t)
			reader, sender, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer sender.Close()
			header := make([]byte, 4)
			body := []byte("{}")
			binary.LittleEndian.PutUint32(header, uint32(len(body)))
			rights := unix.UnixRights(int(sender.Fd()))
			switch mode {
			case "zero-size":
				binary.LittleEndian.PutUint32(header, 0)
			case "too-large":
				binary.LittleEndian.PutUint32(header, proto.MaxMessageBytes+4097)
			case "json":
				body = []byte("!?")
			case "partial-header":
				header, body = header[:2], nil
			case "partial-body":
				body = body[:1]
			case "extra-rights":
				// More rights than the receiver can accept also exercises MSG_CTRUNC.
				rights = unix.UnixRights(int(sender.Fd()), int(sender.Fd()), int(sender.Fd()))
			}
			if _, _, err := right.conn.WriteMsgUnix(append(header, body...), rights, nil); err != nil {
				t.Fatal(err)
			}
			right.close()
			if err := sender.Close(); err != nil {
				t.Fatal(err)
			}
			if _, fd, err := left.receive(); err == nil || fd != -1 {
				t.Fatalf("malformed frame returned fd=%d, err=%v", fd, err)
			}
			pipeEOFForExecutorTest(t, reader)
		})
	}
}

func TestExecutorLinkClosesUnexpectedReplyDescriptor(t *testing.T) {
	left, right := executorPairForTest(t)
	reader, sender, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer sender.Close()
	serveExecutorForTest(t, left, noExecutorRequests)
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := left.call(ctx, executorMessage{Op: "request"}, -1)
		result <- err
	}()
	msg, fd, err := right.receive()
	if err != nil || fd != -1 {
		t.Fatalf("request = %+v, fd=%d, err=%v", msg, fd, err)
	}
	if err := right.send(executorMessage{Reply: true, ID: msg.ID}, int(sender.Fd())); err != nil {
		t.Fatal(err)
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	pipeEOFForExecutorTest(t, reader)
}

func TestExecutorLinkErrorsAreNotReplayed(t *testing.T) {
	left, right := executorPairForTest(t)
	var calls atomic.Int32
	serveExecutorForTest(t, left, noExecutorRequests)
	serveExecutorForTest(t, right, func(msg executorMessage, _ *int) (executorMessage, error) {
		calls.Add(1)
		if msg.Op == "fail" {
			return executorMessage{}, errors.New("request failed")
		}
		return executorMessage{PID: msg.PID}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := left.call(ctx, executorMessage{Op: "fail"}, -1); err == nil || err.Error() != "request failed" {
		t.Fatalf("error reply = %v", err)
	}
	reply, err := left.call(ctx, executorMessage{Op: "echo", PID: 17}, -1)
	if err != nil || reply.PID != 17 {
		t.Fatalf("request after handler error = %+v, %v", reply, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler ran %d times for two requests", got)
	}
}

func TestExecutorLinkTimeoutDoesNotReplay(t *testing.T) {
	left, right := executorPairForTest(t)
	serveExecutorForTest(t, left, noExecutorRequests)
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := left.call(ctx, executorMessage{Op: "first"}, -1)
		result <- err
	}()
	first, _, err := right.receive()
	if err != nil || first.Op != "first" {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	select {
	case <-left.done:
	default:
		t.Fatal("sent request timed out without closing its link")
	}
	left.mu.Lock()
	pending := len(left.pending)
	left.mu.Unlock()
	if pending != 0 {
		t.Fatalf("timed-out request left %d pending waiters", pending)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := left.call(ctx, executorMessage{Op: "second"}, -1); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request after an uncertain outcome did not fail immediately: %v", err)
	}
	if extra, _, err := right.receive(); !errors.Is(err, io.EOF) {
		t.Fatalf("timed-out request was replayed or link remained open: %+v, %v", extra, err)
	}
	// Once an operation's outcome is unknown, a late reply cannot make the
	// execution channel usable again or admit another lifecycle operation.
	if err := right.send(executorMessage{Reply: true, ID: first.ID, PID: 99}, -1); err == nil {
		t.Fatal("late reply succeeded on a failed execution channel")
	}
}

func TestExecutorLinkExpiredContextDoesNotSend(t *testing.T) {
	left, right := executorPairForTest(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := left.call(ctx, executorMessage{Op: "expired"}, -1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired context = %v", err)
	}
	if err := right.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if msg, _, err := right.receive(); err == nil {
		t.Fatalf("expired call sent a request: %+v", msg)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("expired call closed a usable link: %v", err)
	}
	left.mu.Lock()
	pending := len(left.pending)
	left.mu.Unlock()
	if pending != 0 {
		t.Fatalf("expired call left %d pending waiters", pending)
	}
}

func TestExecutorLinkTimeoutWhileWaitingForWriter(t *testing.T) {
	left, right := executorPairForTest(t)
	left.writing <- struct{}{}
	defer func() { <-left.writing }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := left.call(ctx, executorMessage{Op: "queued"}, -1)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued call = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued call ignored its deadline")
	}
	if err := right.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if msg, _, err := right.receive(); err == nil {
		t.Fatalf("timed-out queued call was sent: %+v", msg)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("queued timeout closed a usable link: %v", err)
	}
}

func TestExecutorLinkCanceledPartialSendClosesLink(t *testing.T) {
	left, right := executorPairForTest(t)
	if err := left.conn.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}
	reader, sender, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer sender.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := left.call(ctx, executorMessage{Op: "large", Launch: &proto.LaunchSpec{Exec: "/bin/sh", Env: map[string]string{"LARGE": strings.Repeat("x", 512<<10)}}}, int(sender.Fd()))
		result <- err
	}()
	// Read only the header: rights have transferred, but the body cannot fit
	// in the socket buffer. Cancellation must stop this in-progress write.
	header := make([]byte, 4)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := right.conn.ReadMsgUnix(header, oob)
	if err != nil {
		t.Fatal(err)
	}
	control, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(control) != 1 {
		t.Fatalf("partial send did not transfer rights: %v, %v", control, err)
	}
	rights, err := unix.ParseUnixRights(&control[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range rights {
		defer unix.Close(fd)
	}
	if len(rights) != 1 {
		t.Fatalf("received %d descriptors", len(rights))
	}
	if _, err := io.ReadFull(right.conn, header[n:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled partial send succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("in-progress write ignored cancellation")
	}
	select {
	case <-left.done:
	default:
		t.Fatal("partial send left a reusable link")
	}
	if _, err := unix.FcntlInt(sender.Fd(), unix.F_GETFD, 0); err != nil {
		t.Fatalf("failed send closed caller-owned descriptor: %v", err)
	}
	body, err := io.ReadAll(right.conn)
	if err != nil || len(body) >= int(binary.LittleEndian.Uint32(header)) {
		t.Fatalf("partial frame did not end at link closure: %d bytes, %v", len(body), err)
	}
}

func TestExecutorLinkDisconnectReleasesWaiters(t *testing.T) {
	left, right := executorPairForTest(t)
	serveExecutorForTest(t, left, noExecutorRequests)
	const requests = 8
	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := left.call(ctx, executorMessage{Op: "pending"}, -1)
			results <- err
		}()
	}
	seen := make(map[uint64]bool)
	for i := 0; i < requests; i++ {
		msg, _, err := right.receive()
		if err != nil || msg.ID == 0 || seen[msg.ID] {
			t.Fatalf("duplicate or invalid request: %+v, %v", msg, err)
		}
		seen[msg.ID] = true
	}
	right.close()
	for i := 0; i < requests; i++ {
		select {
		case err := <-results:
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("disconnect was not reported promptly: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("disconnect left a request blocked")
		}
	}
	left.mu.Lock()
	pending := len(left.pending)
	left.mu.Unlock()
	if pending != 0 {
		t.Fatalf("disconnect left %d pending waiters", pending)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := left.call(ctx, executorMessage{Op: "after-close"}, -1); err == nil {
		t.Fatal("closed link accepted another request")
	}
}

func TestOuterPIDFromFD(t *testing.T) {
	openPIDFD := func(pid int) int {
		t.Helper()
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ENOSYS) {
			t.Skip("kernel does not support pidfd_open")
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Close(fd) })
		return fd
	}
	fd := openPIDFD(os.Getpid())
	if pid, err := outerPIDFromFD(fd); err != nil || pid != os.Getpid() {
		t.Fatalf("live pidfd = %d, %v", pid, err)
	}
	ordinary, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if pid, err := outerPIDFromFD(int(ordinary.Fd())); err == nil || pid != 0 {
		t.Fatalf("ordinary descriptor identified a process: %d, %v", pid, err)
	}
	if pid, err := outerPIDFromFD(-1); err == nil || pid != 0 {
		t.Fatalf("invalid descriptor identified a process: %d, %v", pid, err)
	}

	cmd := exec.Command("/bin/sh", "-c", "read value || exit 0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	childFD := openPIDFD(cmd.Process.Pid)
	if pid, err := outerPIDFromFD(childFD); err != nil || pid != cmd.Process.Pid {
		t.Fatalf("child pidfd = %d, %v", pid, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if pid, err := outerPIDFromFD(childFD); err == nil || pid != 0 {
		t.Fatalf("reaped child still identified as %d, %v", pid, err)
	}
}

func TestControlRefreshUnsentTimeoutPreservesStream(t *testing.T) {
	left, right := executorPairForTest(t)
	if err := left.conn.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}
	raw, err := left.conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	filled := 0
	var fillErr error
	if err := raw.Control(func(fd uintptr) {
		buf := make([]byte, 4096)
		for {
			n, err := unix.Write(int(fd), buf)
			if n > 0 {
				filled += n
			}
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			if err != nil {
				fillErr = err
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if fillErr != nil {
		t.Fatal(fillErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := left.sendControl(ctx, executorMessage{Op: "systemd-peer", Epoch: 1}, -1); err == nil {
		t.Fatal("full stream did not time out")
	}
	select {
	case <-left.done:
		t.Fatal("zero-byte refresh timeout closed execution stream")
	default:
	}
	if _, err := io.CopyN(io.Discard, right.conn, int64(filled)); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := left.sendControl(ctx2, executorMessage{Op: "systemd-peer", Epoch: 2}, -1); err != nil {
		t.Fatal(err)
	}
	msg, fd, err := right.receive()
	if err != nil || fd != -1 || msg.Epoch != 2 {
		t.Fatalf("preserved stream corrupted: %+v fd=%d err=%v", msg, fd, err)
	}
}

func TestControlRefreshPartialFrameTimeoutClosesStream(t *testing.T) {
	left, _ := executorPairForTest(t)
	if err := left.conn.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := left.sendControl(ctx, executorMessage{Op: "systemd-peer-fd", Error: strings.Repeat("x", 64*1024)}, -1)
	if err == nil {
		t.Fatal("blocked partial frame did not time out")
	}
	select {
	case <-left.done:
	default:
		t.Fatal("partial frame left a corrupt stream open")
	}
}
