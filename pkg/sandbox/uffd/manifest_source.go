package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
)

// ManifestSnapshotSource implements SnapshotReader against a manifest://
// snapshot bundle. The Fetcher provides chunk-granular random access via
// cache-ctl + store-ctl; this source maps memfd offset → bundle offset
// 1:1 (memory section starts at bundle offset 0, sized RAMSize).
//
// Hole classification comes from the manifest's externally declared
// HoleExtent list — these match the SEEK_DATA / SEEK_HOLE pattern the
// uploader produced from the live memfd, so the lazy-load ratio at
// restore is identical to a SparseSnapshotSource (no extra zero pages
// fetched, no hole pages over-served).
//
// ReadAt caps the run at the next hole boundary (page-aligned),
// mirroring SparseSnapshotSource semantics. zero pages do not generate
// any RPC; data pages cost one Fetcher.ReadAt → one or more cache-ctl
// chunk fetches at chunk granularity.
//
// Safe for concurrent use: the Fetcher is concurrent-safe and ctx is
// shared read-only.
type ManifestSnapshotSource struct {
	fetcher fetch.Stream
	ctx     context.Context
	ramSize uint64
}

// NewManifestSnapshotSource binds a fetcher to the snapshot bundle and
// captures a parent context that scopes all subsequent ReadAt calls
// (the SnapshotReader interface is sync; cancellation goes through this
// stored ctx).
//
// ramSize is the memory section size (= sandbox memory capacity); it
// must be ≤ fetcher.ImageSize() because the bundle = memory + ZIP and
// the source only serves the memory prefix.
func NewManifestSnapshotSource(ctx context.Context, fetcher fetch.Stream, ramSize uint64) (*ManifestSnapshotSource, error) {
	if fetcher == nil {
		return nil, errors.New("uffd: nil fetcher")
	}
	if ramSize == 0 {
		return nil, errors.New("uffd: ramSize must be > 0")
	}
	if ramSize > fetcher.ImageSize() {
		return nil, fmt.Errorf("uffd: ramSize %d exceeds bundle size %d", ramSize, fetcher.ImageSize())
	}
	return &ManifestSnapshotSource{
		fetcher: fetcher,
		ctx:     ctx,
		ramSize: ramSize,
	}, nil
}

// ReadAt implements SnapshotReader.
//
// Classification:
//   - memfdOffset ≥ ramSize → (0, true, io.EOF)
//   - memfdOffset is inside a hole that fully covers the page →
//     (PageSize-aligned hole-run, true, nil), no Fetcher RPC issued
//   - otherwise → (data run from Fetcher.ReadAt, false, nil), capped
//     at the next hole boundary (Fetcher.ReadAt does this internally)
//
// Pages that straddle a hole/data boundary are conservatively classified
// as data; the Fetcher zero-fills the hole portion of the read in
// ReadAtBlock semantics? No — `ReadAt` returns ErrHitHole for offsets
// inside a hole. We never call Fetcher.ReadAt on a page that's inside a
// hole; if a page straddles the boundary we treat it as data and the
// hole portion ends up as data bytes via the ZIP/manifest content (the
// uploader writes those bytes verbatim, including any trailing zeros).
func (s *ManifestSnapshotSource) ReadAt(buf []byte, memfdOffset uint64) (int, bool, error) {
	if memfdOffset >= s.ramSize {
		return 0, true, io.EOF
	}
	avail := s.ramSize - memfdOffset
	if uint64(len(buf)) > avail {
		buf = buf[:avail]
	}
	if len(buf) == 0 {
		return 0, true, nil
	}

	// Hole classification: only count as zero if the entire page sits
	// inside a hole. A partial overlap is treated as data and falls
	// through to Fetcher.ReadAt.
	pageEnd := memfdOffset + PageSize
	if pageEnd > s.ramSize {
		pageEnd = s.ramSize
	}
	if h, in := s.fetcher.FindHole(memfdOffset); in {
		holeEnd := h.Offset + h.Size
		if pageEnd <= holeEnd {
			// Page is entirely in this hole. Extend the run to the
			// hole's end (or buffer/ramSize, whichever first), rounded
			// down to page boundary.
			runEnd := holeEnd
			if runEnd > memfdOffset+uint64(len(buf)) {
				runEnd = memfdOffset + uint64(len(buf))
			}
			if runEnd > s.ramSize {
				runEnd = s.ramSize
			}
			runBytes := runEnd - memfdOffset
			runBytes -= runBytes % PageSize
			if runBytes == 0 {
				runBytes = PageSize // sub-page tail at end-of-image
				if memfdOffset+runBytes > s.ramSize {
					runBytes = s.ramSize - memfdOffset
				}
			}
			return int(runBytes), true, nil
		}
		// Partial overlap — fall through and let Fetcher serve. But
		// Fetcher.ReadAt would refuse with ErrHitHole on this offset.
		// In practice this case is impossible because the uploader's
		// holes are always page-aligned (memfd page granularity), so
		// any page overlapping a hole is fully inside it.
	}

	// Data path. Fetcher.ReadAt caps at the next hole boundary
	// internally, so a single call may return less than len(buf).
	n, err := s.fetcher.ReadAt(s.ctx, buf, memfdOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, fmt.Errorf("manifest snapshot ReadAt at %d: %w", memfdOffset, err)
	}
	// Round n down to a page boundary; tail bytes (if any) are zero-
	// filled by the handler.
	n -= n % PageSize
	if n == 0 {
		// Sub-page tail at end-of-image; serve a single page (handler
		// expects PageSize-aligned ≥ PageSize unless EOF).
		full := int(PageSize)
		if memfdOffset+uint64(full) > s.ramSize {
			full = int(s.ramSize - memfdOffset)
		}
		nn, perr := s.fetcher.ReadAt(s.ctx, buf[:full], memfdOffset)
		if perr != nil && !errors.Is(perr, io.EOF) {
			return 0, false, perr
		}
		// Pad to full page within buffer bounds.
		for i := nn; i < full; i++ {
			buf[i] = 0
		}
		return full, false, nil
	}
	return n, false, nil
}
