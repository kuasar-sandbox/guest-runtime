package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/uffd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/vhost"
)

// lazyStatsTicker periodically logs lazy-load progress so a slow remote store
// or cache (or a backed-up fault queue) is visible in real time. Each printed
// line covers ONLY the interval since the previous line — windowed rates and
// windowed page-in / block-read latency percentiles, never since-start.
//
// Cadence is adaptive: it polls at `fast` (min(base,2s)) while there is
// activity — so a busy or slow phase reports promptly — and backs off to `base`
// (the configured --stats-interval) after a couple of idle polls. A poll whose
// window saw no faults, no reads, and nothing in flight prints nothing, so a
// warm, fully-paged sandbox is silent. The first poll starts in fast mode to
// catch the cold-start / restore page-in burst. Stops when ctx is cancelled.
//
// getUffd returns the uffd handler or nil (constructed asynchronously on the
// first va_report); the ticker tolerates nil until then. Baselines advance
// every poll, so windows are contiguous and idle (zero-delta) gaps lose nothing.
func lazyStatsTicker(ctx context.Context, base time.Duration, getUffd func() *uffd.Handler, srv0, srv1 *vhost.Server, logf func(string, ...any)) {
	fast := min(base, 2*time.Second)
	const idleBackoffPolls = 2 // consecutive idle polls before backing off to base

	sleep := fast
	var prevU uffd.LazyStats
	var prev0, prev1 vhost.StatsSnapshot
	last := time.Now()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		now := time.Now()
		elapsed := now.Sub(last).Seconds()
		if elapsed <= 0 {
			elapsed = sleep.Seconds()
		}

		// Sample, then compute the window against the PREVIOUS baseline
		// before advancing it.
		s0 := srv0.SnapshotStats()
		s1 := srv1.SnapshotStats()
		var u uffd.LazyStats
		if h := getUffd(); h != nil {
			u = h.LazyStats()
		}

		d0, b0 := s0.Read.Count-prev0.Read.Count, s0.Read.Bytes-prev0.Read.Bytes
		d1, b1 := s1.Read.Count-prev1.Read.Count, s1.Read.Bytes-prev1.Read.Bytes
		w0, w1 := s0.Read.Sub(prev0.Read), s1.Read.Sub(prev1.Read)
		faultDelta := (u.FaultsAbsent + u.FaultsReleased + u.FaultsLoaded) -
			(prevU.FaultsAbsent + prevU.FaultsReleased + prevU.FaultsLoaded)
		pageDelta := (u.PagesCopied + u.PagesZeroed) - (prevU.PagesCopied + prevU.PagesZeroed)
		win := u.PageIn.Sub(prevU.PageIn)
		active := faultDelta > 0 || d0 > 0 || d1 > 0 || u.Inflight > 0 || u.QueueDepth > 0

		prevU, prev0, prev1, last = u, s0, s1, now

		if !active {
			idle++
			if idle >= idleBackoffPolls {
				sleep = base
			}
			continue
		}
		idle, sleep = 0, fast
		logf("[lazy] uffd %.0f fault/s %.0f pgin/s inflight=%d queued=%d fetch_p50=%s p99=%s max=%s | %s %.0f rd/s %.1fMB/s p99=%s | %s %.0f rd/s %.1fMB/s p99=%s",
			float64(faultDelta)/elapsed, float64(pageDelta)/elapsed, u.Inflight, u.QueueDepth,
			fmtNs(win.P50()), fmtNs(win.P99()), fmtNs(win.MaxNs),
			s0.Name, float64(d0)/elapsed, float64(b0)/elapsed/1e6, fmtNs(w0.P99()),
			s1.Name, float64(d1)/elapsed, float64(b1)/elapsed/1e6, fmtNs(w1.P99()))
	}
}

// fmtNs renders a nanosecond latency in the largest unit ≤ the value.
func fmtNs(ns uint64) string {
	switch {
	case ns == 0:
		return "0"
	case ns < 1_000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.0fµs", float64(ns)/1e3)
	case ns < 1_000_000_000:
		return fmt.Sprintf("%.1fms", float64(ns)/1e6)
	default:
		return fmt.Sprintf("%.2fs", float64(ns)/1e9)
	}
}
