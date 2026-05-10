package uffd

import (
	"errors"
	"io"
	"math/bits"
	"syscall"
)

var errENXIO = errors.New("uffd: ENXIO (no more data)")

func unixSeek(fd int, off int64, whence int) (int64, error) {
	r, err := syscall.Seek(fd, off, whence)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return 0, errENXIO
		}
		return 0, err
	}
	return r, nil
}

func unixPread(fd int, buf []byte, off int64) (int, error) {
	return syscall.Pread(fd, buf, off)
}

// SnapshotReader supplies the page contents the handler installs at a
// given memfd offset on an Absent fault.
//
// Single-call protocol: ReadAt returns both the run classification
// (zero vs data) AND the run length the source can serve in one
// call, page-aligned and bounded by len(buf). The source picks the
// length to align with its own internal boundaries — chunk edges for
// manifest-backed sources, IsZero classification changes for sparse
// file-backed sources. Handler must accept any returned n; pages
// beyond will be served by separate calls when faulted.
//
// Implementations:
//   - ZeroSource:             cold-start. Always zero, full buf.
//   - SparseSnapshotSource:   restore from local <sid>.snapshot file.
//                             Walks holeMap.
//   - ManifestSnapshotSource: restore from manifest://. Caps at chunk
//                             edges and decrypts via fetch.Fetcher.
//
// Implementations must be safe for concurrent calls.
type SnapshotReader interface {
	// ReadAt fills buf with up to len(buf) bytes starting at memfdOffset
	// and returns the run's classification.
	//
	// Return contract:
	//
	//	n:    page-aligned bytes covered (PageSize ≤ n ≤ len(buf), or 0 at EOF)
	//	zero: true  → source did NOT write to buf; handler installs zero pages
	//	      false → buf[:n] holds plaintext; handler copies them
	//	err:  io.EOF when memfdOffset ≥ source size (n=0); transport otherwise.
	//
	// memfdOffset and len(buf) are guaranteed PageSize-aligned by the
	// handler. Implementations must round n down to a PageSize multiple
	// (zero-padding the partial last page internally on file/network
	// short reads).
	//
	// Safe for concurrent use.
	ReadAt(buf []byte, memfdOffset uint64) (n int, zero bool, err error)
}

// ZeroSource implements SnapshotReader for cold-start. Every page is
// zero; the source never writes to buf.
type ZeroSource struct{}

func (ZeroSource) ReadAt(buf []byte, _ uint64) (int, bool, error) {
	return len(buf), true, nil
}

// SparseSnapshotSource implements SnapshotReader for restore from a
// local <sid>.snapshot file. Backed by [BaseOff, BaseOff+RAMSize) of
// fd, with a pre-built hole bitmap walked from SEEK_DATA / SEEK_HOLE
// at restore-prep time.
type SparseSnapshotSource struct {
	FD      int
	BaseOff int64    // file offset of memfd_offset 0; usually 0
	RAMSize uint64   // total memory section size
	HoleMap []uint64 // bit per (memfd_offset/PageSize): 1 = hole, 0 = data
}

// NewSparseSnapshotSource walks SEEK_DATA / SEEK_HOLE on
// fd[BaseOff..BaseOff+ramSize) to build the hole bitmap. After init,
// classification is O(1) per page.
func NewSparseSnapshotSource(fd int, baseOff int64, ramSize uint64) (*SparseSnapshotSource, error) {
	numPages := (ramSize + PageSize - 1) / PageSize
	mapWords := (numPages + 63) / 64
	holeMap := make([]uint64, mapWords)
	for i := range holeMap {
		holeMap[i] = ^uint64(0)
	}
	const seekData = 3
	const seekHole = 4
	off := baseOff
	end := baseOff + int64(ramSize)
	for off < end {
		dataOff, err := unixSeek(fd, off, seekData)
		if err != nil {
			if err == errENXIO {
				break
			}
			return nil, err
		}
		holeOff, err := unixSeek(fd, dataOff, seekHole)
		if err != nil {
			return nil, err
		}
		if holeOff > end {
			holeOff = end
		}
		startPage := uint64((dataOff - baseOff) / PageSize)
		endPage := uint64((holeOff - baseOff + PageSize - 1) / PageSize)
		if endPage > numPages {
			endPage = numPages
		}
		for p := startPage; p < endPage; p++ {
			holeMap[p/64] &^= 1 << (p % 64)
		}
		off = holeOff
	}
	return &SparseSnapshotSource{
		FD:      fd,
		BaseOff: baseOff,
		RAMSize: ramSize,
		HoleMap: holeMap,
	}, nil
}

// pageIsZero reports whether the page at memfdOffset is in a hole.
// Pages past RAMSize are treated as zero (handler should not request
// them, but be defensive).
func (s *SparseSnapshotSource) pageIsZero(memfdOffset uint64) bool {
	if memfdOffset >= s.RAMSize {
		return true
	}
	page := memfdOffset / PageSize
	if page/64 >= uint64(len(s.HoleMap)) {
		return true
	}
	return s.HoleMap[page/64]&(1<<(page%64)) != 0
}

// runLengthFrom returns the number of consecutive pages starting at
// pageIdx that share zero's classification. Capped at maxPages.
//
// Word-at-a-time scan via bits.TrailingZeros64 / LeadingZeros64
// gives ~64× speedup over per-page bit lookup when the page run
// straddles long zero/data extents.
func (s *SparseSnapshotSource) runLengthFrom(pageIdx, maxPages uint64, zero bool) uint64 {
	if pageIdx >= s.numPages() {
		return 0
	}
	avail := s.numPages() - pageIdx
	if maxPages > avail {
		maxPages = avail
	}
	if maxPages == 0 {
		return 0
	}

	wordIdx := pageIdx / 64
	bitOff := pageIdx % 64

	// Scan within the head word first.
	var w uint64
	if zero {
		// Want consecutive 1 bits → invert and look for trailing zeros.
		w = ^s.HoleMap[wordIdx] >> bitOff
	} else {
		w = s.HoleMap[wordIdx] >> bitOff
	}
	headRun := uint64(bits.TrailingZeros64(w))
	if bitOff+headRun > 64 {
		headRun = 64 - bitOff
	}
	if headRun >= maxPages {
		return maxPages
	}

	// Continue across whole words.
	n := headRun
	wordIdx++
	for n < maxPages && wordIdx < uint64(len(s.HoleMap)) {
		w = s.HoleMap[wordIdx]
		if !zero {
			w = ^w
		}
		// All bits 1 means "all hole/data of the right kind" — full 64 pages.
		if w == ^uint64(0) {
			step := uint64(64)
			if n+step > maxPages {
				step = maxPages - n
			}
			n += step
			wordIdx++
			continue
		}
		// First trailing zero is the run end within this word.
		stop := uint64(bits.TrailingZeros64(^w))
		if n+stop > maxPages {
			stop = maxPages - n
		}
		n += stop
		break
	}
	return n
}

func (s *SparseSnapshotSource) numPages() uint64 {
	return (s.RAMSize + PageSize - 1) / PageSize
}

// ReadAt implements SnapshotReader. Caps n at the next IsZero
// classification change.
func (s *SparseSnapshotSource) ReadAt(buf []byte, memfdOffset uint64) (int, bool, error) {
	if memfdOffset >= s.RAMSize {
		return 0, true, io.EOF
	}
	avail := s.RAMSize - memfdOffset
	if uint64(len(buf)) > avail {
		buf = buf[:avail]
	}
	maxPages := uint64(len(buf)) / PageSize
	if maxPages == 0 {
		maxPages = 1 // sub-page tail at end-of-image
	}

	pageIdx := memfdOffset / PageSize
	zero := s.pageIsZero(memfdOffset)
	runPages := s.runLengthFrom(pageIdx, maxPages, zero)
	if runPages == 0 {
		runPages = 1
	}
	runBytes := runPages * PageSize
	if runBytes > uint64(len(buf)) {
		runBytes = uint64(len(buf))
	}

	if zero {
		return int(runBytes), true, nil
	}

	n, err := unixPread(s.FD, buf[:runBytes], s.BaseOff+int64(memfdOffset))
	if err != nil {
		return 0, false, err
	}
	// Zero-pad on short pread (file shorter than RAMSize).
	for i := n; i < int(runBytes); i++ {
		buf[i] = 0
	}
	return int(runBytes), false, nil
}
