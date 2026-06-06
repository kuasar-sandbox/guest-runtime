package snapshot

import (
	"fmt"
	"io"
	"os"

	"github.com/kuasar-sandbox/sandbox-accelerator/pkg/manifest/codec"
)

// sparseLayer presents [0,size) of a local file as a closable ReadSeeker (an
// io.SectionReader, so a read never spills past size — e.g. into a .snapshot's
// ZIP trailer). Used as the merge BASE: the parent local bundle's memory
// section, or the parent local overlay.
type sparseLayer struct {
	f *os.File
	*io.SectionReader
}

func (s *sparseLayer) Close() error { return s.f.Close() }

// openMergeBase opens a parent local file and returns its [0,size) view plus the
// holes over [0,size). size must not exceed the file (a snapshot's memory
// section size = MemfdSize; an overlay's = the diff size).
func openMergeBase(path string, size int64) (*sparseLayer, []codec.HoleExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if st.Size() < size {
		f.Close()
		return nil, nil, fmt.Errorf("merge base %s: file size %d < expected layer size %d", path, st.Size(), size)
	}
	holes, err := walkHolesCodec(int(f.Fd()), size)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return &sparseLayer{f: f, SectionReader: io.NewSectionReader(f, 0, size)}, holes, nil
}

// mergeSparse flattens two adjacent sparse layers — top (this run's resident
// delta) over base (the local parent snapshot this run was restored from) —
// into a single layer, for the local "replace the next-newest layer" export
// (docs/sandbox.md §3.5). It is the SAVE-side dual of fetch.Layered: at any
// offset top's byte wins where top is resident; where top is a hole, base's
// byte shows through; where BOTH are holes the position stays a (merged) hole
// that falls through to the parent's OWN from_refs / base_from_refs (the
// grandparent chain) at restore.
//
// Semantics MUST match fetch.Layered so that restoring the merged single layer
// is byte-identical to restoring the original [top, base] two-layer chain
// (asserted by merge_test.go). Local sparse files only ever classify as Data or
// Hole (no IsZero — that is a manifest-chunk concept), so a hole-intersection is
// the complete merge rule.
//
// Returns a ReadSeeker over [0,size) plus the merged hole map. The result is
// consumed by the existing sink (AbsorbBundle / AbsorbOverlay), which reads only
// the non-hole data segments — so the merge stays sparse and the sink is unchanged.
func mergeSparse(top io.ReadSeeker, topHoles []codec.HoleExtent, base io.ReadSeeker, baseHoles []codec.HoleExtent, size int64) (io.ReadSeeker, []codec.HoleExtent) {
	m := &mergedReadSeeker{top: top, topHoles: topHoles, base: base, baseHoles: baseHoles, size: size}
	return m, holeIntersection(topHoles, baseHoles, size)
}

type layerKind uint8

const (
	fromTop layerKind = iota
	fromBase
	fromHole
)

// mergedReadSeeker presents (top over base) as a single io.ReadSeeker. pos
// advances monotonically when driven by the sink's writeSparseFile (Seek to a
// data segment, then sequential reads).
type mergedReadSeeker struct {
	top, base           io.ReadSeeker
	topHoles, baseHoles []codec.HoleExtent
	size, pos           int64
}

// ownerAt returns which layer serves pos and the end of its contiguous run
// (≤ size): top where top is resident; base where top is a hole but base is
// resident; hole where both are holes.
func (m *mergedReadSeeker) ownerAt(pos int64) (layerKind, int64) {
	topHole, topEnd := holeRun(pos, m.topHoles, m.size)
	if !topHole {
		return fromTop, topEnd
	}
	baseHole, baseEnd := holeRun(pos, m.baseHoles, m.size)
	end := min(topEnd, baseEnd) // run ends as soon as EITHER layer's state changes
	if !baseHole {
		return fromBase, end
	}
	return fromHole, end
}

func (m *mergedReadSeeker) Read(p []byte) (int, error) {
	if m.pos >= m.size {
		return 0, io.EOF
	}
	owner, end := m.ownerAt(m.pos)
	n := int64(len(p))
	if rem := end - m.pos; n > rem {
		n = rem
	}
	if n <= 0 {
		return 0, io.EOF
	}
	switch owner {
	case fromTop:
		return m.readFrom(m.top, p[:n])
	case fromBase:
		return m.readFrom(m.base, p[:n])
	default: // fromHole — defensive zero-fill (the sink skips merged holes)
		clear(p[:n])
		m.pos += n
		return int(n), nil
	}
}

func (m *mergedReadSeeker) readFrom(layer io.ReadSeeker, p []byte) (int, error) {
	if _, err := layer.Seek(m.pos, io.SeekStart); err != nil {
		return 0, err
	}
	rd, err := io.ReadFull(layer, p)
	m.pos += int64(rd)
	if err == io.ErrUnexpectedEOF {
		err = nil // short read at the layer's end; pos advanced by what we got
	}
	return rd, err
}

func (m *mergedReadSeeker) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = m.pos + off
	case io.SeekEnd:
		abs = m.size + off
	default:
		return 0, fmt.Errorf("merge: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("merge: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// holeRun reports whether pos lies in a hole and the end of the current run
// (hole-end if in a hole, else the start of the next hole, or size). holes must
// be sorted, non-overlapping, within [0,size) (WalkHoles guarantees this).
func holeRun(pos int64, holes []codec.HoleExtent, size int64) (bool, int64) {
	for _, h := range holes {
		hs, he := int64(h.Offset), int64(h.Offset+h.Size)
		if pos < hs {
			return false, hs
		}
		if pos < he {
			return true, he
		}
	}
	return false, size
}

// holeIntersection returns the ranges that are holes in BOTH inputs over
// [0,size) — positions with no data in either layer, which become merged holes
// that fall through to the grandparent chain at restore.
func holeIntersection(a, b []codec.HoleExtent, size int64) []codec.HoleExtent {
	var out []codec.HoleExtent
	for pos := int64(0); pos < size; {
		aHole, aEnd := holeRun(pos, a, size)
		bHole, bEnd := holeRun(pos, b, size)
		end := min(aEnd, bEnd)
		if aHole && bHole {
			if n := len(out); n > 0 && int64(out[n-1].Offset+out[n-1].Size) == pos {
				out[n-1].Size += uint64(end - pos) // coalesce contiguous run
			} else {
				out = append(out, codec.HoleExtent{Offset: uint64(pos), Size: uint64(end - pos)})
			}
		}
		pos = end
	}
	return out
}
