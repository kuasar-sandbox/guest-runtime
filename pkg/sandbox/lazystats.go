package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/uffd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/vhost"
)

// lazyStatsTicker periodically logs lazy-load progress so a slow remote store
// or cache (or a backed-up fault queue) is visible in real time rather than
// only in the on-exit stats dump. Each tick reports, since the previous tick:
// uffd page-in fault/page rates, the live in-flight + queued-fault gauges, the
// page-in FETCH latency distribution (p50/p99/max, cumulative), and per vhost
// backend the read IOPS / throughput / p99. Ticks with no activity since the
// last one are skipped, so a warm, fully-paged sandbox stays quiet. Stops when
// ctx is cancelled (CH exit / run unwind).
//
// getUffd returns the uffd handler or nil (it is constructed asynchronously on
// the first va_report); the ticker tolerates nil until then.
func lazyStatsTicker(ctx context.Context, interval time.Duration, getUffd func() *uffd.Handler, srv0, srv1 *vhost.Server, logf func(string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	secs := interval.Seconds()
	var prevU uffd.LazyStats
	var prev0, prev1 vhost.StatsSnapshot
	haveU := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		s0 := srv0.SnapshotStats()
		s1 := srv1.SnapshotStats()
		d0, b0 := s0.Read.Count-prev0.Read.Count, s0.Read.Bytes-prev0.Read.Bytes
		d1, b1 := s1.Read.Count-prev1.Read.Count, s1.Read.Bytes-prev1.Read.Bytes
		prev0, prev1 = s0, s1

		var faultRate, pageRate uint64
		var inflight int64
		var queued int
		var p50, p99, pmax uint64
		if h := getUffd(); h != nil {
			u := h.LazyStats()
			if haveU {
				faultRate = (u.FaultsAbsent + u.FaultsReleased + u.FaultsLoaded) -
					(prevU.FaultsAbsent + prevU.FaultsReleased + prevU.FaultsLoaded)
				pageRate = (u.PagesCopied + u.PagesZeroed) - (prevU.PagesCopied + prevU.PagesZeroed)
			}
			inflight, queued = u.Inflight, u.QueueDepth
			p50, p99, pmax = u.PageIn.P50(), u.PageIn.P99(), u.PageIn.MaxNs
			prevU, haveU = u, true
		}

		// Quiet once warm: nothing paged or read this interval and nothing in flight.
		if faultRate == 0 && d0 == 0 && d1 == 0 && inflight == 0 && queued == 0 {
			continue
		}
		logf("[lazy] uffd %.0f fault/s %.0f pgin/s inflight=%d queued=%d fetch_p50=%s p99=%s max=%s | %s %.0f rd/s %.1fMB/s p99=%s | %s %.0f rd/s %.1fMB/s p99=%s",
			float64(faultRate)/secs, float64(pageRate)/secs, inflight, queued,
			fmtNs(p50), fmtNs(p99), fmtNs(pmax),
			s0.Name, float64(d0)/secs, float64(b0)/secs/1e6, fmtNs(s0.Read.P99()),
			s1.Name, float64(d1)/secs, float64(b1)/secs/1e6, fmtNs(s1.Read.P99()))
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
