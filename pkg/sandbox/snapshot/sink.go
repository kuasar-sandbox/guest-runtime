package snapshot

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/codec"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/ingest"
	"github.com/fullof-work/mass-sandbox/pkg/store"
	"golang.org/x/sys/unix"
)

// HexKey hex-encodes a content key for use in `manifest://<hex>` refs.
func HexKey(k store.ContentKey) string {
	return hex.EncodeToString(k[:])
}

// SnapshotSink absorbs the two large snapshot artifacts — the blk1 overlay and
// the memory+ZIP bundle — straight from their sources to a destination,
// WITHOUT staging them in /run tmpfs. Two impls share this interface:
//
//   - FileSink writes sparse, content-addressed local files (<sha>.overlay /
//     <sha>.snapshot) under an output dir (--output).
//   - IngestSink streams to a manifest store via ingest.Ingester (--upload).
//
// Each method takes the source as an io.ReadSeeker plus its hole map (the
// caller computes holes via SEEK_HOLE on the source fd); the sink reads only
// resident extents. They return the artifact's ref (file://<sha>.ext |
// manifest://<key>) and, for file mode, its local path ("" for ingest).
type SnapshotSink interface {
	AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []codec.HoleExtent) (ref, path string, err error)
	AbsorbBundle(ctx context.Context, mem io.ReadSeeker, holes []codec.HoleExtent, zip io.Reader) (ref, path string, err error)
}

// ---------------------------------------------------------------------------
// FileSink — sparse local files, content-addressed via hashSparseFile.
// ---------------------------------------------------------------------------

type FileSink struct {
	outDir    string
	sandboxID string
	logf      func(string, ...any)
}

func NewFileSink(outDir, sandboxID string, logf func(string, ...any)) *FileSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &FileSink{outDir: outDir, sandboxID: sandboxID, logf: logf}
}

func (s *FileSink) AbsorbOverlay(_ context.Context, diff io.ReadSeeker, holes []codec.HoleExtent) (string, string, error) {
	digest, final, err := s.writeContentAddressed(diff, holes, nil, "overlay")
	if err != nil {
		return "", "", err
	}
	s.logf("snapshot: %s.overlay written", digest[:12])
	return "file://" + digest + ".overlay", final, nil
}

func (s *FileSink) AbsorbBundle(_ context.Context, mem io.ReadSeeker, holes []codec.HoleExtent, zip io.Reader) (string, string, error) {
	digest, final, err := s.writeContentAddressed(mem, holes, zip, "snapshot")
	if err != nil {
		return "", "", err
	}
	// <sid>.snapshot symlink → the immutable content-addressed name, so
	// file-mode from_refs chains reference <sha>.snapshot (docs/sandbox.md §6.1).
	link := filepath.Join(s.outDir, s.sandboxID+".snapshot")
	_ = os.Remove(link)
	if err := os.Symlink(digest+".snapshot", link); err != nil {
		return "", "", fmt.Errorf("symlink %s.snapshot: %w", s.sandboxID, err)
	}
	s.logf("snapshot: %s.snapshot written", digest[:12])
	return "file://" + digest + ".snapshot", final, nil
}

// writeContentAddressed sparse-copies src's resident extents (+ optional dense
// tail) to a <sid>.<ext>.partial file, hashes it (resident-only via SEEK_DATA),
// then renames to <sha>.<ext>. The hash pass never re-reads the multi-GiB hole
// space.
func (s *FileSink) writeContentAddressed(src io.ReadSeeker, holes []codec.HoleExtent, tail io.Reader, ext string) (string, string, error) {
	size, err := seekerSize(src)
	if err != nil {
		return "", "", err
	}
	tmp := filepath.Join(s.outDir, s.sandboxID+"."+ext+".partial")
	if err := writeSparseFile(tmp, src, size, holes, tail); err != nil {
		return "", "", fmt.Errorf("write %s: %w", ext, err)
	}
	digest, err := hashSparseFile(tmp)
	if err != nil {
		return "", "", fmt.Errorf("hash %s: %w", ext, err)
	}
	final := filepath.Join(s.outDir, digest+"."+ext)
	if err := os.Rename(tmp, final); err != nil {
		return "", "", fmt.Errorf("rename %s: %w", ext, err)
	}
	return digest, final, nil
}

// ---------------------------------------------------------------------------
// IngestSink — manifest store via ingest.Ingester (--upload).
// ---------------------------------------------------------------------------

type IngestSink struct {
	ing  ingest.Ingester
	logf func(string, ...any)
	// Captured for the caller's Response (read via Results after Take).
	overlayRes *ingest.Result
	bundleRes  *ingest.Result
}

func NewIngestSink(ing ingest.Ingester, logf func(string, ...any)) *IngestSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &IngestSink{ing: ing, logf: logf}
}

// Results returns the overlay and bundle ingest results (nil until the
// corresponding Absorb call succeeds). Used by the caller to report stats.
func (s *IngestSink) Results() (overlay, bundle *ingest.Result) {
	return s.overlayRes, s.bundleRes
}

func (s *IngestSink) AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []codec.HoleExtent) (string, string, error) {
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", err
	}
	res, err := s.run(ctx, diff, uint64(size), holes, "overlay")
	if err != nil {
		return "", "", err
	}
	s.overlayRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

func (s *IngestSink) AbsorbBundle(ctx context.Context, mem io.ReadSeeker, holes []codec.HoleExtent, zip io.Reader) (string, string, error) {
	memSize, err := seekerSize(mem)
	if err != nil {
		return "", "", err
	}
	tail, err := io.ReadAll(zip) // ZIP trailer is small (KB)
	if err != nil {
		return "", "", fmt.Errorf("read zip: %w", err)
	}
	// Ingest requires a ReadSeeker when Holes is set (it seeks per data segment
	// → reads resident only). Hand it a seekable view of [mem][zip] so the
	// memory holes are skipped — no tmpfs copy of the bundle.
	src := &concatReadSeeker{mem: mem, memSize: memSize, tail: tail}
	res, err := s.run(ctx, src, uint64(memSize)+uint64(len(tail)), holes, "memory section")
	if err != nil {
		return "", "", err
	}
	s.bundleRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

// run ingests r with a throttled progress log (effective denominator = size
// minus hole bytes, so % reflects real work).
func (s *IngestSink) run(ctx context.Context, r io.Reader, size uint64, holes []codec.HoleExtent, label string) (*ingest.Result, error) {
	var holeBytes uint64
	for _, h := range holes {
		holeBytes += h.Size
	}
	effective := size
	if holeBytes < size {
		effective = size - holeBytes
	}
	const mib = 1 << 20
	start := time.Now()
	lastT := start
	var lastProcessed uint64
	onProgress := func(processed, _ uint64) {
		now := time.Now()
		if now.Sub(lastT) < 2*time.Second {
			return
		}
		rate := float64(processed-lastProcessed) / now.Sub(lastT).Seconds() / mib
		pct := uint64(0)
		if effective > 0 {
			pct = min(processed*100/effective, 100)
		}
		s.logf("upload: %s %d/%d MiB (%d%%) %.0f MiB/s", label, processed/mib, effective/mib, pct, rate)
		lastT, lastProcessed = now, processed
	}
	res, err := s.ing.Ingest(ctx, r, size, ingest.IngestOption{Holes: holes, OnProgress: onProgress})
	if err != nil {
		return nil, fmt.Errorf("ingest %s: %w", label, err)
	}
	s.logf("upload: %s ingested key=%x stored=%d dedup=%d in %.1fs",
		label, res.ManifestKey, res.StoredChunks, res.DedupChunks, time.Since(start).Seconds())
	return res, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// seekerSize returns rs's length and rewinds it to the start.
func seekerSize(rs io.ReadSeeker) (int64, error) {
	n, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return n, nil
}

// writeSparseFile creates a sparse file at path of logical size = size (+ tail
// bytes appended after size), copying only src's resident extents (the
// complement of holes); hole ranges are left unwritten (sparse). tail (the ZIP
// trailer, may be nil) is written dense immediately after the size-th byte.
func writeSparseFile(path string, src io.ReadSeeker, size int64, holes []codec.HoleExtent, tail io.Reader) error {
	dst, err := os.Create(path)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := dst.Truncate(size); err != nil {
		return err
	}
	for _, seg := range dataSegments(size, holes) {
		if _, err := src.Seek(int64(seg.Offset), io.SeekStart); err != nil {
			return err
		}
		if _, err := dst.Seek(int64(seg.Offset), io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, int64(seg.Size)); err != nil {
			return err
		}
	}
	if tail != nil {
		if _, err := dst.Seek(size, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(dst, tail); err != nil {
			return err
		}
	}
	return dst.Sync()
}

// dataSegments returns the resident runs = [0,size) minus holes. holes must be
// sorted, non-overlapping, within [0,size) (WalkHoles guarantees this).
func dataSegments(size int64, holes []codec.HoleExtent) []codec.HoleExtent {
	var segs []codec.HoleExtent
	cursor := uint64(0)
	for _, h := range holes {
		if h.Offset > cursor {
			segs = append(segs, codec.HoleExtent{Offset: cursor, Size: h.Offset - cursor})
		}
		cursor = h.Offset + h.Size
	}
	if cursor < uint64(size) {
		segs = append(segs, codec.HoleExtent{Offset: cursor, Size: uint64(size) - cursor})
	}
	return segs
}

// walkHolesCodec returns fd's holes over [0,size) as codec.HoleExtent (the type
// the sink + ingest consume). Thin adapter over WalkHoles.
func walkHolesCodec(fd int, size int64) ([]codec.HoleExtent, error) {
	hs, err := WalkHoles(fd, size)
	if err != nil {
		return nil, err
	}
	out := make([]codec.HoleExtent, len(hs))
	for i, h := range hs {
		out[i] = codec.HoleExtent{Offset: h.Offset, Size: h.Length}
	}
	return out, nil
}

// residentBytes = size minus the sum of hole sizes.
func residentBytes(size int64, holes []codec.HoleExtent) uint64 {
	var holeBytes uint64
	for _, h := range holes {
		holeBytes += h.Size
	}
	if holeBytes >= uint64(size) {
		return 0
	}
	return uint64(size) - holeBytes
}

// fdReaderAt is a non-owning io.ReaderAt over a raw fd (pread); used to present
// the memfd as an io.ReadSeeker (via io.SectionReader) without an *os.File
// wrapper whose finalizer would close the shared fd.
type fdReaderAt int

func (fd fdReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := unix.Pread(int(fd), p, off)
	if err != nil {
		return n, err
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

// memfdReader presents memfd[0,size) as an io.ReadSeeker (pread-backed, no fd
// ownership). CH is paused during snapshot, so the memfd content is stable.
func memfdReader(fd int, size int64) io.ReadSeeker {
	return io.NewSectionReader(fdReaderAt(fd), 0, size)
}

// concatReadSeeker presents [mem (0..memSize)] followed by [tail] as one
// io.ReadSeeker, so ingest.Ingest can Seek to each data segment across the
// memory/ZIP boundary (skipping memory holes → resident-only reads).
type concatReadSeeker struct {
	mem     io.ReadSeeker
	memSize int64
	tail    []byte
	pos     int64
}

func (c *concatReadSeeker) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = c.pos + off
	case io.SeekEnd:
		abs = c.memSize + int64(len(c.tail)) + off
	default:
		return 0, fmt.Errorf("concat: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("concat: negative position %d", abs)
	}
	c.pos = abs
	if abs < c.memSize {
		if _, err := c.mem.Seek(abs, io.SeekStart); err != nil {
			return 0, err
		}
	}
	return abs, nil
}

func (c *concatReadSeeker) Read(p []byte) (int, error) {
	total := c.memSize + int64(len(c.tail))
	if c.pos >= total {
		return 0, io.EOF
	}
	if c.pos < c.memSize {
		if maxN := c.memSize - c.pos; int64(len(p)) > maxN {
			p = p[:maxN]
		}
		n, err := c.mem.Read(p)
		c.pos += int64(n)
		if err == io.EOF {
			err = nil // memory section ended; the tail still follows
		}
		return n, err
	}
	n := copy(p, c.tail[c.pos-c.memSize:])
	c.pos += int64(n)
	return n, nil
}
