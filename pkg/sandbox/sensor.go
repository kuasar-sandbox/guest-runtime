package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/nodectl"
)

// PressureSensor watches the per-sandbox cgroup for memory pressure
// signals and turns them into RequestBudget calls against the
// controller. See docs/sandbox.md §10.3 for the signal catalogue.
//
// The sensor runs on a 100 ms tick. Per tick it reads:
//
//	memory.events.local      counts high / oom transitions
//	memory.current           current cgroup RSS
//
// Cumulative deltas drive the urgency:
//
//	new oom event   → urgency=high   (emergency_pool path)
//	new high event  → urgency=normal (reserve_pool path)
//	current/high>0.95 + rising slope → urgency=low (predictive)
//
// All RPC calls are non-blocking with respect to the tick: a failed
// call simply logs and waits for the next opportunity. Successful
// grants are applied via ControllerHooks.OnAllocatableChanged so cgroup
// memory.high and CH balloon target stay consistent.
type PressureSensor struct {
	hooks         *ControllerHooks
	cgroupPath    string
	logf          func(string, ...any)
	tick          time.Duration
	step          uint64

	// Cumulative event counts at the previous tick.
	lastHigh atomic.Uint64
	lastOOM  atomic.Uint64

	// Sliding window of memory.current samples for slope detection.
	prevRSS uint64
	prevAt  time.Time
}

// NewPressureSensor builds a sensor pinned to a cgroup directory. Step
// is the per-RequestBudget delta — small enough to keep cooldowns
// useful, big enough to cover a typical pressure spike.
func NewPressureSensor(hooks *ControllerHooks, cgroupPath string, step uint64, logf func(string, ...any)) *PressureSensor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &PressureSensor{
		hooks:      hooks,
		cgroupPath: cgroupPath,
		logf:       logf,
		tick:       100 * time.Millisecond,
		step:       step,
	}
}

// Run is the per-sandbox sensor goroutine entry point. Returns when ctx
// is cancelled. Designed to be called as `go sensor.Run(ctx)` after
// Settled — running it before Settled is harmless but does nothing
// useful (no controller has been told the sandbox is past startup).
func (s *PressureSensor) Run(ctx context.Context) {
	if s == nil || s.hooks == nil || !s.hooks.Enabled() {
		return
	}
	if s.cgroupPath == "" {
		// Mode A — no cgroup, no events to read. The sensor is a noop;
		// burst path becomes deflate_on_oom only.
		return
	}
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.tickOnce(now)
		}
	}
}

func (s *PressureSensor) tickOnce(now time.Time) {
	high, oom, ok := readMemoryEvents(s.cgroupPath)
	if !ok {
		return
	}
	rss := readMemoryCurrent(s.cgroupPath)
	memHigh := readUintFromCgFile(s.cgroupPath, "memory.high")

	prevHigh := s.lastHigh.Swap(high)
	prevOOM := s.lastOOM.Swap(oom)

	dHigh := uint64(0)
	dOOM := uint64(0)
	if high > prevHigh {
		dHigh = high - prevHigh
	}
	if oom > prevOOM {
		dOOM = oom - prevOOM
	}

	// Slope: positive RSS rate compared to last sample.
	rising := false
	if !s.prevAt.IsZero() && rss > s.prevRSS {
		rising = true
	}
	s.prevRSS = rss
	s.prevAt = now

	urgency := ""
	reason := ""
	switch {
	case dOOM > 0:
		urgency = nodectl.UrgencyHigh
		reason = "oom_event"
	case dHigh > 0:
		urgency = nodectl.UrgencyNormal
		reason = "high_event"
	case memHigh > 0 && rss > 0 && rising && rssRatio(rss, memHigh) > 0.95:
		urgency = nodectl.UrgencyLow
		reason = "predicted"
	}
	if urgency == "" {
		return
	}

	currentAlloc := s.hooks.AllocatableNowMem()
	g, newAlloc, _, err := s.hooks.client.RequestBudget(currentAlloc, s.step, urgency, reason)
	if err != nil {
		// EAGAIN-ish: log + try again next tick. Errors here are common
		// during controller restart; not fatal.
		if !isExpectedRPCErr(err) {
			s.logf("sensor: request_budget urgency=%s: %v", urgency, err)
		}
		return
	}
	if g == 0 {
		return
	}
	if err := s.hooks.OnAllocatableChanged(newAlloc); err != nil {
		s.logf("sensor: apply allocatable %d: %v", newAlloc, err)
		return
	}
	s.logf("sensor: granted +%d (urgency=%s reason=%s) → allocatable=%d",
		g, urgency, reason, newAlloc)
}

func rssRatio(rss, high uint64) float64 {
	if high == 0 {
		return 0
	}
	return float64(rss) / float64(high)
}

// readMemoryEvents parses cgroup memory.events.local in the form:
//
//	low 0
//	high 12
//	max 0
//	oom 0
//	oom_kill 0
//
// Returns (high, oom, ok). ok=false means the file was not readable.
func readMemoryEvents(cgroupPath string) (high, oom uint64, ok bool) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.events.local"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		var v uint64
		for _, b := range fields[1] {
			if b < '0' || b > '9' {
				v = 0
				break
			}
			v = v*10 + uint64(b-'0')
		}
		switch fields[0] {
		case "high":
			high = v
		case "oom", "oom_kill":
			if v > oom {
				oom = v
			}
		}
	}
	return high, oom, true
}

// readUintFromCgFile reads a single decimal uint from a cgroup file.
// Returns 0 on read error or "max" content.
func readUintFromCgFile(cgroupPath, name string) uint64 {
	data, err := os.ReadFile(filepath.Join(cgroupPath, name))
	if err != nil {
		return 0
	}
	var n uint64
	for _, b := range data {
		if b < '0' || b > '9' {
			break
		}
		n = n*10 + uint64(b-'0')
	}
	return n
}

func isExpectedRPCErr(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		errors.Is(err, os.ErrClosed)
}

// StartSensor spawns the pressure sensor goroutine. Idempotent — safe
// to call after Settled.
func (h *ControllerHooks) StartSensor(ctx context.Context, step uint64) {
	if !h.Enabled() {
		return
	}
	sensor := NewPressureSensor(h, h.opts.CgroupPath, step, h.opts.Logf)
	bgCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if h.cancelBg == nil {
		h.cancelBg = cancel
	} else {
		// Heartbeat already owns cancelBg; chain so Release cancels both.
		old := h.cancelBg
		h.cancelBg = func() { old(); cancel() }
	}
	h.mu.Unlock()
	h.bgWG.Add(1)
	go func() {
		defer h.bgWG.Done()
		sensor.Run(bgCtx)
	}()
}
