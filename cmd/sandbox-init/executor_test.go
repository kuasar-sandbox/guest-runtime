package main

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func TestExecutorHandoffDoesNotShutdownSocket(t *testing.T) {
	outer, inner := executorPairForTest(t)
	received := make(chan *vsockConn, 1)
	serveExecutorForTest(t, outer, noExecutorRequests)
	serveExecutorForTest(t, inner, func(_ executorMessage, fd *int) (executorMessage, error) {
		conn, err := takeExecutorConn(fd)
		if err == nil {
			received <- conn
		}
		return executorMessage{}, err
	})
	guest, host := usageSocketPair(t)
	client := &executorClient{link: outer}
	handed, err := client.transfer(context.Background(), executorMessage{Op: "session"}, guest)
	if !handed || err != nil {
		t.Fatalf("handoff: %v, %v", handed, err)
	}
	owner := <-received
	t.Cleanup(func() { _ = owner.Close() })
	if !guest.closed {
		t.Fatal("sender retained connection ownership")
	}
	// A stale cleanup by the original owner must remain a no-op even after
	// its descriptor number has become available for reuse.
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Write([]byte("inner")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(host, buf); err != nil || string(buf) != "inner" {
		t.Fatalf("new owner cannot write after release: %q, %v", buf, err)
	}
	if _, err := host.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(owner, buf); err != nil || string(buf) != "reply" {
		t.Fatalf("new owner cannot read after release: %q, %v", buf, err)
	}
}

func TestExecutorLaunchFinishesBeforeQuiesce(t *testing.T) {
	outer, inner := executorPairForTest(t)
	entered, release, quiesced := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	serveExecutorForTest(t, outer, noExecutorRequests)
	serveExecutorForTest(t, inner, func(msg executorMessage, fd *int) (executorMessage, error) {
		switch msg.Op {
		case "launch":
			close(entered)
			<-release
		case "quiesce":
			close(quiesced)
		default:
			return executorMessage{}, errors.New("unexpected request")
		}
		return executorMessage{}, nil
	})
	client := &executorClient{link: outer, spec: &proto.LaunchSpec{Exec: "primary"}}
	guest, _ := usageSocketPair(t)
	launched := make(chan error, 1)
	go func() { launched <- client.launch(guest) }()
	waitUsageDone(t, entered)
	finished := make(chan error, 1)
	go func() { finished <- client.quiesce() }()
	select {
	case <-quiesced:
		t.Fatal("quiesce crossed an unconfirmed launch handoff")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-launched; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !guest.closed {
		t.Fatal("quiesce completed before launch FD release")
	}
	refused, peer := usageSocketPair(t)
	handed, err := client.runSession(refused, &proto.Message{Type: proto.TypeExec})
	if handed || err != nil {
		t.Fatalf("frozen execution admission = %v, %v", handed, err)
	}
	msg, err := proto.ReadMessage(peer)
	if err != nil || msg.Type != proto.TypeError {
		t.Fatalf("paused session rejection = %+v, %v", msg, err)
	}
}

func TestExecutorQuiesceJoinsReceivedSessionCleanup(t *testing.T) {
	for _, kind := range []string{proto.TypeExec, proto.TypeConnect} {
		t.Run(kind, func(t *testing.T) {
			_, sup, _ := usageReattachFixture(t)
			if err := sup.prepareReattach(); err != nil {
				t.Fatal(err)
			}
			guest, host := usageSocketPair(t)
			// Hold one admitted I/O owner to delay the final descriptor close.
			// The deliberately invalid request exits before either specialized
			// registry admits it, reproducing the pre-registration handoff gap.
			guest.ioWG.Add(1)
			var once sync.Once
			release := func() { once.Do(guest.ioWG.Done) }
			t.Cleanup(release)
			if handed, err := sup.runSession(guest, &proto.Message{Type: kind}); !handed || err != nil {
				t.Fatalf("receipt: %v, %v", handed, err)
			}
			msg, err := proto.ReadMessage(host)
			if err != nil || msg.Type != proto.TypeError {
				t.Fatalf("invalid request response: %+v, %v", msg, err)
			}
			finished := make(chan error, 1)
			go func() { finished <- sup.quiesce() }()
			select {
			case err := <-finished:
				t.Fatalf("quiesce returned before received FD cleanup: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			release()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("quiesce did not finish after session cleanup")
			}
		})
	}
}

func TestExecutorReattachThawsBeforePrivateRPC(t *testing.T) {
	_, sup, bridge := usageReattachFixture(t)
	outer, inner := executorPairForTest(t)
	var thawed atomic.Bool
	server := &innerExecutor{supervisor: sup}
	serveExecutorForTest(t, outer, noExecutorRequests)
	serveExecutorForTest(t, inner, func(msg executorMessage, fd *int) (executorMessage, error) {
		if !thawed.Load() {
			return executorMessage{}, errors.New("requested readiness from frozen inner")
		}
		return server.handle(msg, fd)
	})
	client := &executorClient{link: outer, spec: &proto.LaunchSpec{}, paused: true}
	guest, host := usageSocketPair(t)
	handed, err := finishReattach(guest, client, 17, true, func() error {
		thawed.Store(true)
		return nil
	})
	if !handed || err != nil {
		t.Fatalf("reattach: %v, %v", handed, err)
	}
	t.Cleanup(func() { closeUsageTestMUX(t, bridge, host) })
	msg, err := proto.ReadMessage(host)
	if err != nil || msg.Type != proto.TypeRestoreAck || msg.Epoch != 17 {
		t.Fatalf("ACK must precede inner MUX frames: %+v, %v", msg, err)
	}
	if bridge.holder.peek() == nil || guestMemReports.isPaused() || client.paused || sup.quiescing.Load() {
		t.Fatal("ACK/MUX completion did not establish execution and observation readiness")
	}
}

func TestExecutorShutdownRejectsLaterLifecycleChanges(t *testing.T) {
	for _, kind := range []string{proto.TypeQuiesce, proto.TypeAttach, proto.TypeRestore} {
		t.Run(kind, func(t *testing.T) {
			guest, host := usageSocketPair(t)
			client := &executorClient{}
			client.stopping.Store(true)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if !handleReverseConn(guest, client) {
					_ = guest.Close()
				}
			}()
			if err := proto.WriteMessage(host, &proto.Message{Type: kind}); err != nil {
				t.Fatal(err)
			}
			msg, err := proto.ReadMessage(host)
			if err != nil || msg.Type != proto.TypeError || msg.Msg != "sandbox is shutting down" {
				t.Fatalf("post-shutdown lifecycle: %+v, %v", msg, err)
			}
			waitUsageDone(t, done)
		})
	}
}

func TestExecDuringPrimaryRestartGapDoesNotFork(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	sup := &supervisorState{execReg: newExecRegistry()}
	pid, waiter, err := sup.forkExec(&proto.ExecSpec{Argv: []string{"/bin/true"}}, childStdio{stdout: writer})
	if pid != 0 || waiter != nil || err == nil {
		t.Fatalf("exec fell through to another namespace: pid=%d waiter=%v err=%v", pid, waiter, err)
	}
	if _, err := unix.FcntlInt(writer.Fd(), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("failed exec retained child stdio: %v", err)
	}
}

func TestExecutorReattachStdioMatchesEstablishedChannels(t *testing.T) {
	for _, tty := range []bool{false, true} {
		spec := proto.StdioSpec{TTY: tty, Winsize: &proto.Winsize{Cols: 80, Rows: 24}, Stdin: true, Stdout: true, Stderr: true}
		client := &executorClient{spec: &proto.LaunchSpec{Stdio: spec}}
		bridge := &consoleBridge{tty: tty, hasStdin: true, hasStdout: true, hasStderr: true}
		if got, want := client.stdioSpec(), bridge.protoSpec(); got != want {
			t.Fatalf("reattach stdio = %+v, want %+v", got, want)
		}
	}
}
