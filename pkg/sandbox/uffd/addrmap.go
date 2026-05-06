package uffd

import "sync"

// ProcessKind labels which process's VMA recorded an entry.
type ProcessKind uint8

const (
	// ProcessBackend = sandbox-ctl's mmap of the memfd (registered
	// for uffd at startup; receives backend-side fault sources like
	// pkg/vhost-induced direct accesses on backendVA).
	ProcessBackend ProcessKind = 1
	// ProcessCH = cloud-hypervisor's mmap of the same memfd. Reported
	// to us via the va_report handshake at CH boot.
	ProcessCH ProcessKind = 2
)

// vma holds one virtual-memory-area record.
type vma struct {
	process ProcessKind
	start   uint64
	end     uint64
}

// AddressMap translates a fault VA (from any registered VMA) into the
// memfd-relative offset. Both backendVA and chVA cover the same memfd
// inode, so a fault at either VA → memfd_offset = offsetWithinVMA.
//
// Stable in steady state (RLock per fault). Only mutated when CH reports
// its own VA at startup (one-time RegisterVMA from the va_report server).
type AddressMap struct {
	mu       sync.RWMutex
	memfdLen uint64 // covers the entire memfd
	vmas     []vma
}

// NewAddressMap creates an empty map sized for a memfd of memfdLen
// bytes. RegisterVMA can be called any number of times; typical use:
// once at sandbox-ctl startup for ProcessBackend, once on va_report ack
// for ProcessCH.
func NewAddressMap(memfdLen uint64) *AddressMap {
	return &AddressMap{memfdLen: memfdLen}
}

// RegisterVMA records that [vaStart, vaStart+size) maps the entire
// memfd at offset 0. Returns nil on success or an error if size doesn't
// match memfdLen (we only support whole-zone mappings; sub-range mmap
// is not part of the unified-memfd model).
func (m *AddressMap) RegisterVMA(p ProcessKind, vaStart uint64, size uint64) error {
	if size != m.memfdLen {
		return errSizeMismatch{want: m.memfdLen, got: size}
	}
	m.mu.Lock()
	m.vmas = append(m.vmas, vma{process: p, start: vaStart, end: vaStart + size})
	m.mu.Unlock()
	return nil
}

// Locate returns (memfdOffset, true) if faultVA falls within any
// registered VMA, else (_, false).
func (m *AddressMap) Locate(faultVA uint64) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.vmas {
		v := &m.vmas[i]
		if faultVA >= v.start && faultVA < v.end {
			return faultVA - v.start, true
		}
	}
	return 0, false
}

// VMAStart returns the start VA of the VMA owned by the given process,
// or (0, false) if none registered. Used by the EVENT_REMOVE handler to
// translate the CH-side range to the backend-side range for the
// reciprocal madvise(DONTNEED, backendVA).
func (m *AddressMap) VMAStart(p ProcessKind) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.vmas {
		if m.vmas[i].process == p {
			return m.vmas[i].start, true
		}
	}
	return 0, false
}

type errSizeMismatch struct {
	want, got uint64
}

func (e errSizeMismatch) Error() string {
	return formatSizeMismatch(e.want, e.got)
}

func formatSizeMismatch(want, got uint64) string {
	return "uffd: AddressMap RegisterVMA size mismatch: want " +
		formatHex(want) + " got " + formatHex(got)
}

func formatHex(v uint64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0x0"
	}
	var buf [18]byte
	buf[0] = '0'
	buf[1] = 'x'
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}
