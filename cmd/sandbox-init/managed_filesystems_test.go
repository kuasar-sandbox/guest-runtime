package main

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func addManagedTestFS(t *testing.T, r *managedFilesystems, disk string, dev uint64) *os.File {
	t.Helper()
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.add(disk, f, dev); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestManagedFilesystemErrorSurvivesRetry(t *testing.T) {
	var r managedFilesystems
	addManagedTestFS(t, &r, "root", 1)
	calls := 0
	fn := func(int) error {
		calls++
		if calls == 1 {
			return unix.EIO
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := r.syncWith(fn); !errors.Is(err, unix.EIO) {
			t.Fatalf("capture %d lost writeback error: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("failed generation retried syncfs %d times", calls)
	}
}

func TestManagedFilesystemDeduplicatesPinnedSuperblock(t *testing.T) {
	var r managedFilesystems
	first := addManagedTestFS(t, &r, "root", 1)
	duplicate := addManagedTestFS(t, &r, "same-root-bind", 1)
	if _, err := duplicate.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("duplicate FD retained: %v", err)
	}
	calls := 0
	if err := r.syncWith(func(fd int) error {
		calls++
		if fd != int(first.Fd()) {
			t.Fatalf("error cursor replaced: fd=%d", fd)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("syncfs called %d times for one superblock", calls)
	}
}

func TestManagedFilesystemRejectsIdentityReplacement(t *testing.T) {
	var r managedFilesystems
	addManagedTestFS(t, &r, "disk-0", 1)
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.add("disk-0", f, 2); err == nil {
		t.Fatal("replaced a live filesystem identity")
	}
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed registration leaked FD: %v", err)
	}
}

func TestQuiesceStorageFailureNeverDropsCaches(t *testing.T) {
	for _, skip := range []bool{false, true} {
		got, err := runQuiesceWith(skip, func() error { return unix.EIO }, func(string, []byte) error {
			t.Fatal("cache drop ran after failed filesystem sync")
			return nil
		})
		if got != proto.DropCachesSkipped || !errors.Is(err, unix.EIO) {
			t.Fatalf("got %s, %v", got, err)
		}
	}
}

func TestSlotCaptureGateDrainsAndResumesMountWriters(t *testing.T) {
	var slots diskSlots
	slots.capture.RLock()
	paused := make(chan struct{})
	go func() { slots.pauseMounts(); close(paused) }()
	select {
	case <-paused:
		t.Fatal("capture crossed admitted mount writer")
	case <-time.After(20 * time.Millisecond):
	}
	slots.capture.RUnlock()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("mount drain did not finish")
	}
	admitted := make(chan struct{})
	go func() { slots.capture.RLock(); close(admitted); slots.capture.RUnlock() }()
	select {
	case <-admitted:
		t.Fatal("mount admitted during capture")
	case <-time.After(20 * time.Millisecond):
	}
	slots.resumeMounts()
	slots.resumeMounts() // ordinary attach and recovery retries are idempotent
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("mount did not resume")
	}
}
