package main

import (
	"syscall"
	"testing"
	"time"
)

func TestBackoffNext(t *testing.T) {
	var b backoff
	now := time.Unix(0, 0)
	quick := now.Add(time.Millisecond) // exit ~1ms after start → keep doubling

	b.onStart(now)
	want := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond,
		80 * time.Millisecond, 160 * time.Millisecond,
	}
	for i, w := range want {
		if got := b.next(quick); got != w {
			t.Errorf("next[%d] = %v, want %v", i, got, w)
		}
	}

	// Keep failing fast → caps at backoffMax.
	for i := 0; i < 20; i++ {
		b.next(quick)
	}
	if got := b.next(quick); got != backoffMax {
		t.Errorf("capped delay = %v, want %v", got, backoffMax)
	}

	// Stayed up ≥ backoffReset before exiting → reset to backoffMin.
	b.onStart(now)
	if got := b.next(now.Add(backoffReset)); got != backoffMin {
		t.Errorf("reset delay = %v, want %v", got, backoffMin)
	}
	// ...and resume doubling from there.
	if got := b.next(quick); got != 2*backoffMin {
		t.Errorf("post-reset delay = %v, want %v", got, 2*backoffMin)
	}
}

func TestWantRestart(t *testing.T) {
	exit0 := syscall.WaitStatus(0)      // clean exit (code 0)
	exit1 := syscall.WaitStatus(1 << 8) // exit code 1
	killed := syscall.WaitStatus(9)     // signalled (SIGKILL)

	cases := []struct {
		policy string
		st     syscall.WaitStatus
		want   bool
	}{
		{"always", exit0, true},
		{"always", exit1, true},
		{"on-failure", exit0, false},
		{"on-failure", exit1, true},
		{"on-failure", killed, true},
		{"never", exit1, false},
		{"", exit1, false},
		{"", exit0, false},
	}
	for _, c := range cases {
		if got := wantRestart(c.policy, c.st); got != c.want {
			t.Errorf("wantRestart(%q, %#x) = %v, want %v", c.policy, uint32(c.st), got, c.want)
		}
	}
}
