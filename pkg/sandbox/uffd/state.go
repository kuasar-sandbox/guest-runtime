package uffd

import "sync"

// PageState packed in a byte.
type PageState uint8

const (
	StateAbsent   PageState = 0
	StateLoaded   PageState = 1
	StateReleased PageState = 2
)

// PageStateMap holds one byte per page. Concurrent access is guarded by
// a single RWMutex — Get/Set/CAS hold the lock briefly. With per-page
// hashing in the worker dispatcher (same page idx routes to same
// worker), same-page contention is rare; cross-page concurrent updates
// are common but each worker's update is microseconds.
type PageStateMap struct {
	mu    sync.RWMutex
	pages []uint8
}

// NewPageStateMap allocates a state table for n pages, all StateAbsent.
func NewPageStateMap(n int) *PageStateMap {
	return &PageStateMap{pages: make([]uint8, n)}
}

// Get returns the current state of pageIdx.
func (m *PageStateMap) Get(pageIdx uint64) PageState {
	if pageIdx >= uint64(len(m.pages)) {
		return StateAbsent
	}
	m.mu.RLock()
	v := m.pages[pageIdx]
	m.mu.RUnlock()
	return PageState(v)
}

// Set unconditionally writes the new state.
func (m *PageStateMap) Set(pageIdx uint64, s PageState) {
	if pageIdx >= uint64(len(m.pages)) {
		return
	}
	m.mu.Lock()
	m.pages[pageIdx] = uint8(s)
	m.mu.Unlock()
}

// CompareAndSwap returns true if the old state was old and the new
// value was stored.
func (m *PageStateMap) CompareAndSwap(pageIdx uint64, old, new PageState) bool {
	if pageIdx >= uint64(len(m.pages)) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pages[pageIdx] != uint8(old) {
		return false
	}
	m.pages[pageIdx] = uint8(new)
	return true
}

// SetRange writes state to every page in [startIdx, endIdx).
func (m *PageStateMap) SetRange(startIdx, endIdx uint64, s PageState) {
	if endIdx > uint64(len(m.pages)) {
		endIdx = uint64(len(m.pages))
	}
	v := uint8(s)
	m.mu.Lock()
	for i := startIdx; i < endIdx; i++ {
		m.pages[i] = v
	}
	m.mu.Unlock()
}

// Len returns the number of tracked pages.
func (m *PageStateMap) Len() int { return len(m.pages) }
