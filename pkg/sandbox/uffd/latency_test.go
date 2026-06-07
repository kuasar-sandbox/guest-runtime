package uffd

import "testing"

func TestLatHist_Empty(t *testing.T) {
	var h latHist
	s := h.snapshot()
	if s.Count != 0 || s.P50() != 0 || s.P99() != 0 || s.MaxNs != 0 {
		t.Fatalf("empty hist not zero: %+v p50=%d p99=%d", s, s.P50(), s.P99())
	}
}

func TestLatHist_CountSumMax(t *testing.T) {
	var h latHist
	samples := []uint64{1_000, 2_000, 500_000, 3_000_000} // 1µs,2µs,500µs,3ms
	var sum uint64
	for _, v := range samples {
		h.record(v)
		sum += v
	}
	s := h.snapshot()
	if s.Count != uint64(len(samples)) {
		t.Errorf("count = %d, want %d", s.Count, len(samples))
	}
	if s.SumNs != sum {
		t.Errorf("sum = %d, want %d", s.SumNs, sum)
	}
	if s.MaxNs != 3_000_000 {
		t.Errorf("max = %d, want 3000000", s.MaxNs)
	}
}

func TestLatHist_Percentiles(t *testing.T) {
	var h latHist
	// 95 fast (~1µs) + 5 slow (~800ms): p50 stays in the fast mode, while the
	// 5% slow tail pushes p99 into the slow bucket.
	for range 95 {
		h.record(1_000)
	}
	for range 5 {
		h.record(800_000_000)
	}
	s := h.snapshot()
	if p50 := s.P50(); p50 > 10_000 {
		t.Errorf("p50 = %dns, want ≤10µs (dominated by the fast mode)", p50)
	}
	// p99 should land in the slow tail, and never exceed the observed max.
	p99 := s.P99()
	if p99 < 1_000_000 {
		t.Errorf("p99 = %dns, want ≥1ms (must catch the slow tail)", p99)
	}
	if p99 > s.MaxNs {
		t.Errorf("p99 = %dns exceeds max %dns", p99, s.MaxNs)
	}
}

func TestLatSnapshot_Sub_NewPeak(t *testing.T) {
	var h latHist
	for range 10 {
		h.record(1_000) // 10 @ 1µs
	}
	prev := h.snapshot()
	for range 5 {
		h.record(500_000_000) // 5 @ 500ms — a new peak this window
	}
	w := h.snapshot().Sub(prev)
	if w.Count != 5 {
		t.Errorf("window count = %d, want 5 (only the new samples)", w.Count)
	}
	// The window excludes the old fast samples, so p50 sits in the slow band.
	if w.P50() < 100_000_000 {
		t.Errorf("window p50 = %dns, want ≥100ms (old 1µs samples must not count)", w.P50())
	}
	if w.MaxNs != 500_000_000 {
		t.Errorf("window max = %dns, want exactly 500ms (new peak)", w.MaxNs)
	}
}

func TestLatSnapshot_Sub_NoNewPeak(t *testing.T) {
	var h latHist
	h.record(800_000_000) // cumulative peak 800ms
	prev := h.snapshot()
	for range 3 {
		h.record(2_000) // 3 @ 2µs — no new peak this window
	}
	w := h.snapshot().Sub(prev)
	if w.Count != 3 {
		t.Errorf("window count = %d, want 3", w.Count)
	}
	// No new peak → window max falls back to the highest non-empty bucket bound
	// (2µs), NOT the stale cumulative 800ms.
	if w.MaxNs != 2_000 {
		t.Errorf("window max = %dns, want 2000 (2µs bucket bound, not the cumulative peak)", w.MaxNs)
	}
}

func TestLatHist_Overflow(t *testing.T) {
	var h latHist
	h.record(5_000_000_000) // 5s → overflow bucket (>1s)
	s := h.snapshot()
	if s.Buckets[numLatencyBuckets] != 1 {
		t.Errorf("overflow bucket = %d, want 1", s.Buckets[numLatencyBuckets])
	}
	if s.P99() != s.MaxNs {
		t.Errorf("p99 of single overflow sample = %d, want max %d", s.P99(), s.MaxNs)
	}
}
