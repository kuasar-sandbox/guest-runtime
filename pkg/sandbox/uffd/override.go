package uffd

import "sync"

// OverrideMap holds backend-supplied page contents that the handler
// will install when the matching page faults next time. The single-
// uffd architecture eliminates direct backend writes to memfd folios:
// for any page still Absent, vhost-side writes go here; the next
// uffdC fault for that page consumes the override and resolves via
// UFFDIO_COPY (atomic folio + PTE install in CH's mm), so no
// folio-creation race can occur outside the handler.
//
// Backed by a sharded map: 64 mutexes keyed by pageIdx & 63 to
// localize contention when many vhost workers fan out across pages.
// Sharding is enough for the cold-start path (20-30 K page writes,
// dozens of concurrent workers); a sync.Map would suffice but loses
// the explicit Pop semantics we need for fault-time hand-off.
type OverrideMap struct {
	shards [64]overrideShard
}

type overrideShard struct {
	mu sync.Mutex
	m  map[uint64][]byte
}

// NewOverrideMap returns an empty map ready for concurrent use.
func NewOverrideMap() *OverrideMap {
	o := &OverrideMap{}
	for i := range o.shards {
		o.shards[i].m = make(map[uint64][]byte)
	}
	return o
}

func (o *OverrideMap) shard(pageIdx uint64) *overrideShard {
	return &o.shards[pageIdx&63]
}

// Set stores data under pageIdx. Overrides any existing entry — the
// most recent backend write wins (vhost flushes per-request and the
// guest disk-cache layer above us guarantees the latest write is the
// one the guest expects).
//
// data must remain stable until the matching Pop / Drop returns; the
// map keeps the slice header verbatim, no copy.
func (o *OverrideMap) Set(pageIdx uint64, data []byte) {
	s := o.shard(pageIdx)
	s.mu.Lock()
	s.m[pageIdx] = data
	s.mu.Unlock()
}

// Pop atomically reads and removes the entry for pageIdx. Returns the
// stored slice and true on hit, nil and false on miss. The handler's
// Absent-fault path calls this once per faulting page.
func (o *OverrideMap) Pop(pageIdx uint64) ([]byte, bool) {
	s := o.shard(pageIdx)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[pageIdx]
	if ok {
		delete(s.m, pageIdx)
	}
	return d, ok
}

// Drop discards any entry for pageIdx without consuming it. Used by
// EVENT_REMOVE (madvise DONTNEED / balloon free): once the kernel has
// thrown the page away, any pending override is stale.
func (o *OverrideMap) Drop(pageIdx uint64) {
	s := o.shard(pageIdx)
	s.mu.Lock()
	delete(s.m, pageIdx)
	s.mu.Unlock()
}

// Len returns the number of entries currently held. Diagnostics only.
func (o *OverrideMap) Len() int {
	n := 0
	for i := range o.shards {
		o.shards[i].mu.Lock()
		n += len(o.shards[i].m)
		o.shards[i].mu.Unlock()
	}
	return n
}
