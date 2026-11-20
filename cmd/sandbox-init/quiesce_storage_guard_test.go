package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

type storageGuardExecution struct {
	executionControl
	calls atomic.Int32
}

func (e *storageGuardExecution) quiesce() error {
	e.calls.Add(1)
	return errors.New("unexpected inner RPC")
}

func TestQuiesceManagementRejectsStickyStorageErrorBeforeInnerRPC(t *testing.T) {
	// Management tests use serial fixtures. Preserve the existing registry
	// and restore its error state after the socket goroutines have joined.
	guestFilesystems.mu.Lock()
	oldFailure := guestFilesystems.failure
	guestFilesystems.failure = unix.EIO
	guestFilesystems.mu.Unlock()
	t.Cleanup(func() {
		guestFilesystems.mu.Lock()
		guestFilesystems.failure = oldFailure
		guestFilesystems.mu.Unlock()
	})
	for _, skip := range []bool{false, true} {
		execution := &storageGuardExecution{}
		guest, host := usageSocketPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if !handleReverseConn(guest, execution) {
				_ = guest.Close()
			}
		}()
		if err := proto.WriteMessage(host, &proto.Message{Type: proto.TypeQuiesce, SkipDropCaches: skip}); err != nil {
			t.Fatal(err)
		}
		msg, err := proto.ReadMessage(usageIO{host, time.Now().Add(time.Second)})
		if err != nil || msg.Type != proto.TypeError || !strings.Contains(msg.Msg, unix.EIO.Error()) {
			t.Fatalf("poisoned generation response: %+v, %v", msg, err)
		}
		waitUsageDone(t, done)
		if execution.calls.Load() != 0 {
			t.Fatalf("contacted inner before rejecting storage failure: %d", execution.calls.Load())
		}
	}
}

func TestManagedSyncSerializesWritebackErrorCursor(t *testing.T) {
	var r managedFilesystems
	addManagedTestFS(t, &r, "root", 1)
	entered, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	first, second := make(chan error, 1), make(chan error, 1)
	var calls atomic.Int32
	var once sync.Once
	go func() {
		defer close(firstDone)
		first <- r.syncWith(func(int) error {
			calls.Add(1)
			close(entered)
			<-release
			return unix.EIO
		})
	}()
	t.Cleanup(func() { once.Do(func() { close(release) }); waitUsageDone(t, firstDone) })
	waitUsageDone(t, entered)
	secondStarted := make(chan struct{})
	go func() {
		defer close(secondDone)
		close(secondStarted)
		second <- r.syncWith(func(int) error { calls.Add(1); return nil })
	}()
	t.Cleanup(func() { once.Do(func() { close(release) }); waitUsageDone(t, secondDone) })
	waitUsageDone(t, secondStarted)
	select {
	case err := <-second:
		t.Fatalf("concurrent sync advanced cursor before first completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	waitUsageDone(t, firstDone)
	waitUsageDone(t, secondDone)
	if a, b := <-first, <-second; !errors.Is(a, unix.EIO) || !errors.Is(b, unix.EIO) || calls.Load() != 1 {
		t.Fatalf("cursor race lost sticky error: first=%v second=%v syscalls=%d", a, b, calls.Load())
	}
}
