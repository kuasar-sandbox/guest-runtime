package sandbox

import (
	"context"
	"errors"
	"io"

	"github.com/fullof-work/mass-sandbox/pkg/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
)

// ManifestSnapshotSource is a uffd.SnapshotReader backed by a
// fetch.Fetcher. ReadAt caps each call at the chunk that contains
// memfdOffset so a single fault never triggers more than one
// chunk-fetch RPC + decrypt. For zero regions (manifest holes or
// IsZero chunks) the run extends through the contiguous zero span
// without any source I/O.
//
// The reader is intended for `sandbox-ctl run --restore=manifest://...`
// — one fetch.Fetcher constructed against the snapshot bundle's memory
// section, wrapped here, plugged into uffd.Config.
type ManifestSnapshotSource struct {
	fetcher *fetch.Fetcher
	ctx     context.Context
}

// NewManifestSnapshotSource wraps fetcher as a uffd snapshot source.
// ctx scopes asynchronous chunk fetches; cancelling it makes pending
// uffd-driven reads fail promptly during sandbox shutdown.
func NewManifestSnapshotSource(ctx context.Context, fetcher *fetch.Fetcher) *ManifestSnapshotSource {
	return &ManifestSnapshotSource{fetcher: fetcher, ctx: ctx}
}

// Compile-time check: ManifestSnapshotSource implements the uffd contract.
var _ uffd.SnapshotReader = (*ManifestSnapshotSource)(nil)

// ReadAt implements uffd.SnapshotReader. The returned run length is
// capped at the chunk boundary that contains memfdOffset (data
// classification) or at the next non-zero region (zero
// classification), whichever comes first.
//
// Page alignment: memfdOffset and len(buf) are page-aligned by the
// handler. n is rounded down to a PageSize multiple; the partial
// last page near image end is zero-padded internally so the handler
// can UFFDIO_COPY the full page without separate logic.
func (s *ManifestSnapshotSource) ReadAt(buf []byte, memfdOffset uint64) (int, bool, error) {
	imageSize := s.fetcher.ImageSize()
	if memfdOffset >= imageSize {
		return 0, true, io.EOF
	}

	// Cap buf to remaining image.
	avail := imageSize - memfdOffset
	if uint64(len(buf)) > avail {
		buf = buf[:avail]
	}

	zero, runEnd := s.classifyAndRunEnd(memfdOffset, memfdOffset+uint64(len(buf)))
	runBytes := runEnd - memfdOffset

	// Round runBytes down to PageSize so the handler can UFFDIO_*
	// directly. The unaligned tail (<PageSize) only happens at the
	// very end of the image, where the handler caller already passes
	// at most one page anyway.
	if runBytes >= uffd.PageSize {
		runBytes -= runBytes % uffd.PageSize
	} else {
		// runBytes < PageSize: must be the last page near EOF.
		runBytes = uint64(len(buf)) // ≤ one page; zero-padded below for non-zero
	}

	if zero {
		return int(runBytes), true, nil
	}

	// Data run: fetch via Fetcher. ReadAtBlock zero-fills any holes
	// within the range (defensive; classifyAndRunEnd should have
	// stopped at the hole boundary already).
	n, err := s.fetcher.ReadAtBlock(s.ctx, buf[:runBytes], memfdOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	// Zero-pad if Fetcher returned short (image ends inside this run).
	for i := n; i < int(runBytes); i++ {
		buf[i] = 0
	}
	return int(runBytes), false, nil
}

// classifyAndRunEnd determines the classification at memfdOffset and
// the offset of the first byte that breaks the run. The returned run
// is bounded by capEnd.
//
// Rules:
//   - if memfdOffset is in a hole: zero run extends through the hole
//     (and any subsequent holes / IsZero chunks adjacent to it)
//   - if memfdOffset is in an IsZero chunk: zero run extends through
//     consecutive zero regions
//   - if memfdOffset is in a data chunk: data run is capped at the
//     end of that chunk so a single ReadAt triggers at most one
//     chunk fetch
func (s *ManifestSnapshotSource) classifyAndRunEnd(memfdOffset, capEnd uint64) (zero bool, end uint64) {
	m := s.fetcher.Manifest()

	// Hole at head?
	if h, inHole := s.fetcher.FindHole(memfdOffset); inHole {
		end = extendZeroRun(m, h.Offset+h.Size, capEnd)
		return true, end
	}

	// Otherwise must be in a chunk.
	idx := manifest.ChunkIndexForOffset(m.Entries, memfdOffset)
	if idx < 0 {
		// Defensive: geometry says entries+holes tile [0, ImageSize),
		// so this is unreachable. Fall back to a one-page zero run
		// rather than infinite-looping the handler.
		return true, minU64(memfdOffset+uffd.PageSize, capEnd)
	}
	entry := m.Entries[idx]
	chunkEnd := entry.Offset + uint64(entry.Size)

	if entry.IsZero {
		end = extendZeroRun(m, chunkEnd, capEnd)
		return true, end
	}

	// Data chunk — cap at chunk end (single-fetch invariant).
	if chunkEnd > capEnd {
		chunkEnd = capEnd
	}
	return false, chunkEnd
}

// extendZeroRun keeps growing the run as long as the next region
// (chunk or hole) starting at runEnd is also zero-classified.
// Bounded by capEnd. Used after a hole or IsZero-chunk has been
// consumed up to runEnd.
func extendZeroRun(m *manifest.Manifest, runEnd, capEnd uint64) uint64 {
	for runEnd < capEnd {
		// Hole at runEnd?
		if h, inHole := findHoleAt(m, runEnd); inHole {
			runEnd = h.Offset + h.Size
			continue
		}
		// Zero chunk at runEnd?
		idx := manifest.ChunkIndexForOffset(m.Entries, runEnd)
		if idx < 0 {
			break
		}
		entry := m.Entries[idx]
		if !entry.IsZero {
			break
		}
		runEnd = entry.Offset + uint64(entry.Size)
	}
	if runEnd > capEnd {
		runEnd = capEnd
	}
	return runEnd
}

// findHoleAt returns the hole that contains off (if any). Linear
// scan; manifest hole counts are small (single-digit to low-double-
// digit per typical sandbox snapshot).
func findHoleAt(m *manifest.Manifest, off uint64) (manifest.HoleExtent, bool) {
	for _, h := range m.Holes {
		if off >= h.Offset && off < h.Offset+h.Size {
			return h, true
		}
	}
	return manifest.HoleExtent{}, false
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
