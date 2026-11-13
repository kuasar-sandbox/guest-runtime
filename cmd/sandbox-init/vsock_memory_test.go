package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMemoryRetryBudgetAndProgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		n        int
		err      error
		attempts int
	}{
		{"exhausted", -1, syscall.ENOMEM, 8}, {"positive", 3, syscall.ENOMEM, 1},
		{"other-error", 0, syscall.EIO, 1}, {"success", 4, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var delays []time.Duration
			n, err := retryMemoryIO(func() (int, error) { calls++; return tc.n, tc.err }, func(d time.Duration) error { delays = append(delays, d); return nil })
			if n != tc.n || !errors.Is(err, tc.err) || calls != tc.attempts {
				t.Fatalf("result=(%d,%v), calls=%d", n, err, calls)
			}
			var total time.Duration
			for i, d := range delays {
				if d != time.Millisecond<<i {
					t.Fatal(delays)
				}
				total += d
			}
			if tc.attempts == 8 && (len(delays) != 7 || total != 127*time.Millisecond) {
				t.Fatal(delays, total)
			}
			if tc.attempts == 1 && len(delays) != 0 {
				t.Fatal("retried a positive or final result")
			}
		})
	}
}

func TestVsockMemoryRetryRechecksClosedFD(t *testing.T) {
	source, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(source)
	owned, err := unix.FcntlInt(uintptr(source), unix.F_DUPFD_CLOEXEC, 10000)
	if err != nil {
		t.Fatal(err)
	}
	c := &vsockConn{fd: owned}
	var calls atomic.Int32
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.io(func(int, []byte) (int, error) {
			if calls.Add(1) == 1 {
				close(first)
			}
			return 0, syscall.ENOMEM
		}, []byte("x"))
		done <- err
	}()
	<-first
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup3(source, owned, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(owned)
	atClose := calls.Load()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not join shutdown")
	}
	if calls.Load() != atClose {
		t.Fatal("operation touched a reused fd")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0); err != nil {
		t.Fatal("late close touched reused fd:", err)
	}
}

func TestVsockMemoryRetryWaitWakesOnClose(t *testing.T) {
	fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := &vsockConn{fd: fd}
	done := make(chan error, 1)
	go func() { done <- c.waitMemoryRetry(time.Hour) }()
	// Either the timer is already waiting, or it observes closed on entry.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff blocked Close")
	}
}

func TestUsageMemoryRetryPreservesAbsoluteDeadline(t *testing.T) {
	for _, tc := range []string{"eventual-success", "partial", "deadline", "expired", "close"} {
		t.Run(tc, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[1])
			conn := &vsockConn{fd: fds[0]}
			defer conn.Close()
			end := time.Now().Add(time.Second)
			if tc == "deadline" {
				end = time.Now().Add(10 * time.Millisecond)
			}
			if tc == "expired" {
				end = time.Now().Add(-time.Second)
			}
			var calls atomic.Int32
			first := make(chan struct{})
			if tc == "close" {
				go func() { <-first; conn.Close() }()
			}
			n, err := (usageIO{conn: conn, end: end}).io(func(int, []byte) (int, error) {
				count := calls.Add(1)
				if count == 1 {
					close(first)
				}
				if tc == "partial" {
					return 2, syscall.ENOMEM
				}
				if tc == "eventual-success" && count == 3 {
					return 4, nil
				}
				return -1, syscall.ENOMEM
			}, []byte("data"))
			switch tc {
			case "eventual-success":
				if n != 4 || err != nil || calls.Load() != 3 {
					t.Fatalf("%d %v calls=%d", n, err, calls.Load())
				}
			case "partial":
				if n != 2 || !errors.Is(err, syscall.ENOMEM) || calls.Load() != 1 {
					t.Fatalf("positive progress replayed: %d %v calls=%d", n, err, calls.Load())
				}
			case "deadline", "expired":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				if calls.Load() >= 8 || tc == "expired" && calls.Load() != 0 {
					t.Fatalf("deadline renewed, calls=%d", calls.Load())
				}
			case "close":
				if !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
			}
		})
	}
}
