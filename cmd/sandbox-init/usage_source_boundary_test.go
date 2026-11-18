package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// These are producer ownership tests. A consumer in the user PID namespace
// cannot inspect or ptrace the outer init's worker or retained filesystem FD.
// Use the real service/connection/reattach paths and a blocked source read to
// make the source's completion order deterministic without a product hook.
func TestUsageSourceLateResultAcrossConnectionAndReattach(t *testing.T) {
	for _, mode := range []string{"reconnect", "attach", "restore", "failed-attach-retry"} {
		t.Run(mode, func(t *testing.T) {
			s, sup, bridge := usageReattachFixture(t)
			_, generation := s.resume(false)
			s.commitResume(generation)
			blocked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var reads atomic.Int32
			s.sources = append(s.sources, &usageSource{disk: "blocked", incarnation: "blocked/1", read: func() usageRawResult {
				value := uint64(222)
				if reads.Add(1) == 1 {
					close(blocked)
					<-release
					value = 111 // Belongs only to the now-finished original request.
				}
				return usageRawResult{filesystem: proto.UsageFilesystem{
					Disk: "blocked", Incarnation: "blocked/1", Blocks: usageUint(value),
					UsageReadState: proto.UsageReadState{Status: proto.UsageOK},
				}}
			}})

			client, connectionDone := usageManagementConn(t, sup, bridge)
			first := readUsageResponse(t, client, "old-host", 101)
			waitUsageDone(t, blocked)
			if first.Filesystems[1].Status != proto.UsageTimeout || reads.Load() != 1 {
				t.Fatalf("first read = %+v, starts=%d", first.Filesystems[1], reads.Load())
			}
			_ = client.shutdown()
			_ = client.Close()
			waitUsageDone(t, connectionDone)

			epoch, id := "old-host", uint64(102)
			if mode != "reconnect" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := s.pause(ctx); err != nil {
					t.Fatal(err)
				}
				if mode == "failed-attach-retry" {
					guest, _ := usageSocketPair(t)
					injected := errors.New("injected thaw failure")
					handed, err := finishReattach(guest, sup, 1, false, func() error { return injected })
					if handed || !errors.Is(err, injected) || reads.Load() != 1 {
						t.Fatalf("failed reattach: handed=%t err=%v starts=%d", handed, err, reads.Load())
					}
					_ = guest.Close()
				}
				newHost := mode == "restore"
				guest, host := usageSocketPair(t)
				handed, err := finishReattach(guest, sup, 2, newHost, func() error { return nil })
				if err != nil || !handed {
					t.Fatalf("reattach: handed=%t err=%v", handed, err)
				}
				ack, err := proto.ReadMessage(host)
				wantAck := proto.TypeAttachAck
				if newHost {
					wantAck, epoch, id = proto.TypeRestoreAck, "new-host", 1
				}
				if err != nil || ack.Type != wantAck {
					t.Fatalf("reattach ACK = %+v, %v", ack, err)
				}
				t.Cleanup(func() { closeUsageTestMUX(t, bridge, host) })
			}

			client, _ = usageManagementConn(t, sup, bridge)
			for round := 0; round < 4; round++ {
				r := readUsageResponse(t, client, epoch, id)
				id++
				if r.Filesystems[1].Status != proto.UsageBusy || reads.Load() != 1 {
					t.Fatalf("occupied source replaced after %s: %+v, starts=%d", mode, r.Filesystems[1], reads.Load())
				}
			}
			releaseOnce.Do(func() { close(release) })
			deadline := time.Now().Add(time.Second)
			for {
				r := readUsageResponse(t, client, epoch, id)
				id++
				value := r.Filesystems[1]
				if value.Status == proto.UsageOK {
					if value.Blocks == nil || *value.Blocks != 222 || reads.Load() < 2 {
						t.Fatalf("late result reused by epoch=%s id=%d: %+v", r.RunEpoch, r.RequestID, value)
					}
					break
				}
				if value.Status != proto.UsageBusy || time.Now().After(deadline) {
					t.Fatalf("source did not recover with a new read: %+v", value)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestUsageFilesystemHandleOwnership(t *testing.T) {
	if fdText := os.Getenv("SANDBOX_USAGE_FD_HELPER"); fdText != "" {
		fd, err := strconv.Atoi(fdText)
		if err != nil {
			os.Exit(90)
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && fmt.Sprintf("%d:%d", st.Dev, st.Ino) == os.Getenv("SANDBOX_USAGE_FD_ID") {
			os.Exit(91)
		}
		return
	}
	dir := t.TempDir()
	s := newUsageService()
	if err := s.register("disk-0", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	fd := -1
	for _, entry := range entries {
		if path, err := os.Readlink("/proc/self/fd/" + entry.Name()); err == nil && path == dir {
			if fd >= 0 {
				t.Fatal("register retained duplicate directory handles")
			}
			fd, err = strconv.Atoi(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if fd < 0 {
		t.Fatal("register did not retain a filesystem handle")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("registered handle flags=%#x, err=%v", flags, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageFilesystemHandleOwnership$")
	cmd.Env = append(os.Environ(), "SANDBOX_USAGE_FD_HELPER="+strconv.Itoa(fd),
		fmt.Sprintf("SANDBOX_USAGE_FD_ID=%d:%d", st.Dev, st.Ino))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("managed filesystem handle survived exec: %v\n%s", err, out)
	}

	source := s.sources[1]
	read, closeHandle := source.read, source.close
	blocked, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	status := make(chan string, 1)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var closes atomic.Int32
	source.read = func() usageRawResult {
		close(blocked)
		<-release
		result := read() // The real retained FD must still be valid here.
		status <- result.filesystem.Status
		return result
	}
	source.close = func() {
		closes.Add(1)
		closeHandle()
		close(closed)
	}
	r := s.collect(proto.UsageRequest{RunEpoch: "one", RequestID: 1, ReadBudgetNS: int64(20 * time.Millisecond)}, nil)
	waitUsageDone(t, blocked)
	if r.Filesystems[0].Status != proto.UsageTimeout {
		t.Fatalf("blocked source = %+v", r.Filesystems[0])
	}
	s.close()
	s.close()
	if closes.Load() != 0 {
		t.Fatal("service closed a handle still owned by its blocked read")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatalf("blocked read lost its filesystem handle: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	waitUsageDone(t, closed)
	if got := <-status; got != proto.UsageOK && got != proto.UsageUnsupported {
		t.Fatalf("real fstatfs lost its retained handle: %s", got)
	}
	if closes.Load() != 1 {
		t.Fatalf("filesystem handle closed %d times", closes.Load())
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("completed owner left its handle open: %v", err)
	}
	if source.start(1, make(chan usageRawResult, 1)) {
		t.Fatal("closed source accepted a replacement worker")
	}
}
