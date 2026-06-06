package uffd

import "sync/atomic"

// latencyBucketsNs are the inclusive upper bounds of the page-in latency
// histogram buckets, covering 1µs..1s exponentially; samples above 1s land in
// an implicit overflow bucket at index len(latencyBucketsNs). The scheme
// mirrors pkg/vhost so uffd page-in and vhost block latency read the same way.
var latencyBucketsNs = [...]uint64{
	1_000,         // 1µs
	2_000,         // 2µs
	4_000,         // 4µs
	8_000,         // 8µs
	16_000,        // 16µs
	32_000,        // 32µs
	64_000,        // 64µs
	128_000,       // 128µs
	256_000,       // 256µs
	512_000,       // 512µs
	1_000_000,     // 1ms
	2_000_000,     // 2ms
	4_000_000,     // 4ms
	8_000_000,     // 8ms
	16_000_000,    // 16ms
	32_000_000,    // 32ms
	64_000_000,    // 64ms
	128_000_000,   // 128ms
	256_000_000,   // 256ms
	512_000_000,   // 512ms
	1_000_000_000, // 1s
}

const numLatencyBuckets = len(latencyBucketsNs)

// latHist is a lock-free latency histogram (count + sum + max + buckets),
// safe for concurrent record from every fault worker.
type latHist struct {
	count   atomic.Uint64
	sumNs   atomic.Uint64
	maxNs   atomic.Uint64
	buckets [numLatencyBuckets + 1]atomic.Uint64 // [n] = overflow (>1s)
}

func (h *latHist) record(latNs uint64) {
	h.count.Add(1)
	h.sumNs.Add(latNs)
	for {
		m := h.maxNs.Load()
		if latNs <= m {
			break
		}
		if h.maxNs.CompareAndSwap(m, latNs) {
			break
		}
	}
	bkt := numLatencyBuckets
	for i, b := range latencyBucketsNs {
		if latNs <= b {
			bkt = i
			break
		}
	}
	h.buckets[bkt].Add(1)
}

// latSnapshot is an immutable view of a latHist.
type latSnapshot struct {
	Count   uint64
	SumNs   uint64
	MaxNs   uint64
	Buckets [numLatencyBuckets + 1]uint64
}

func (h *latHist) snapshot() latSnapshot {
	s := latSnapshot{Count: h.count.Load(), SumNs: h.sumNs.Load(), MaxNs: h.maxNs.Load()}
	for i := range h.buckets {
		s.Buckets[i] = h.buckets[i].Load()
	}
	return s
}

// Percentile estimates the requested percentile (0..1) by linear interpolation
// across the buckets, clamped to the observed max. Returns 0 with no samples.
// Same algorithm as pkg/vhost ReqSnapshot.Percentile.
func (s latSnapshot) Percentile(p float64) uint64 {
	var total uint64
	for _, v := range s.Buckets {
		total += v
	}
	if total == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	} else if p > 1 {
		p = 1
	}
	target := uint64(float64(total) * p)
	if target == 0 {
		target = 1
	}
	var cum, prevBound, result uint64
	for i, v := range s.Buckets {
		next := cum + v
		var bound uint64
		if i < numLatencyBuckets {
			bound = latencyBucketsNs[i]
		} else if s.MaxNs > 0 {
			bound = s.MaxNs
		} else {
			bound = latencyBucketsNs[numLatencyBuckets-1] * 2
		}
		if next >= target {
			if v == 0 {
				result = bound
			} else {
				into := target - cum
				result = prevBound + (bound-prevBound)*into/v
			}
			break
		}
		cum = next
		prevBound = bound
	}
	if s.MaxNs > 0 && result > s.MaxNs {
		return s.MaxNs
	}
	return result
}

func (s latSnapshot) P50() uint64 { return s.Percentile(0.50) }
func (s latSnapshot) P99() uint64 { return s.Percentile(0.99) }
