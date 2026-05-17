package snapshot

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/codec"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/ingest"
	"github.com/fullof-work/mass-sandbox/pkg/store"
)

// UploadSources gathers the inputs needed to ingest overlay + snapshot
// bundle. Both paths point at files already produced or referenced by
// Take into the staging dir. Ingester is supplied by the caller; one
// shared instance handles both the overlay and the snapshot ingest so
// the underlying store connection pool is reused.
type UploadSources struct {
	OverlayPath   string             // local blk1.diff (live; quiesce-stable)
	SnapshotPath  string             // local <sid>.snapshot from Take (memory + ZIP at end)
	SnapshotHoles []codec.HoleExtent // hole extents inside SnapshotPath (memory section only)
	Ingester      ingest.Ingester    // write pipeline; supplied by caller
	Logf          func(string, ...any)
}

// UploadResult summarises the bytes/dedup metrics for both ingests.
type UploadResult struct {
	OverlayKey          store.ContentKey
	SnapshotKey         store.ContentKey
	OverlayStoredBytes  uint64
	OverlayDedupChunks  uint32
	OverlayTotalChunks  uint32
	SnapshotStoredBytes uint64
	SnapshotDedupChunks uint32
	SnapshotTotalChunks uint32
	WallclockUploadMs   int64
}

// Upload ingests blk1.diff (the live overlay) then patches the
// snapshot.cfg embedded in <sid>.snapshot to point overlay.base at
// `manifest://<overlay-key>`, then ingests <sid>.snapshot.
//
// The two manifests must reside in the same target store (single
// `--upload` switch in CLI). Two-step ingest order matters:
//  1. Overlay ingest first → overlay_key known.
//  2. Patch snapshot.cfg's overlay.base in the bundle's trailing ZIP.
//  3. Snapshot ingest with hole map preserved.
func Upload(ctx context.Context, src UploadSources) (*UploadResult, error) {
	logf := src.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if src.Ingester == nil {
		return nil, fmt.Errorf("upload: Ingester required")
	}
	t0 := time.Now()

	// 1. Ingest blk1.diff (the live overlay; quiesce-stable).
	overlayKey, overlayRes, err := ingestFile(ctx, src.Ingester, src.OverlayPath, nil, "overlay", logf)
	if err != nil {
		return nil, fmt.Errorf("upload overlay: %w", err)
	}
	logf("upload: overlay ingested key=%x stored=%d dedup=%d total=%d",
		overlayKey, overlayRes.StoredChunks, overlayRes.DedupChunks, overlayRes.StoredChunks+overlayRes.DedupChunks)

	// 2. Patch snapshot.cfg inside <sid>.snapshot's trailing ZIP so
	//    overlay.base references manifest://<overlayKey>. The patch is a
	//    "read ZIP entry → YAML edit → re-encode trailing ZIP". The
	//    memory section stays untouched; the sparse layout is preserved.
	overlayRef := "manifest://" + HexKey(overlayKey)
	if err := rewriteSnapshotCfgInBundle(src.SnapshotPath, overlayRef); err != nil {
		return nil, fmt.Errorf("upload: rewrite snapshot.cfg: %w", err)
	}
	logf("upload: snapshot.cfg in bundle patched: overlay.base=%s", overlayRef)

	// 3. Ingest <sid>.snapshot with hole map (memory section's holes).
	snapKey, snapRes, err := ingestFile(ctx, src.Ingester, src.SnapshotPath, src.SnapshotHoles, "memory section", logf)
	if err != nil {
		return nil, fmt.Errorf("upload snapshot: %w", err)
	}
	logf("upload: snapshot ingested key=%x stored=%d dedup=%d total=%d",
		snapKey, snapRes.StoredChunks, snapRes.DedupChunks, snapRes.StoredChunks+snapRes.DedupChunks)

	return &UploadResult{
		OverlayKey:          overlayKey,
		SnapshotKey:         snapKey,
		OverlayStoredBytes:  overlayRes.StoredBytes,
		OverlayDedupChunks:  overlayRes.DedupChunks,
		OverlayTotalChunks:  overlayRes.StoredChunks + overlayRes.DedupChunks,
		SnapshotStoredBytes: snapRes.StoredBytes,
		SnapshotDedupChunks: snapRes.DedupChunks,
		SnapshotTotalChunks: snapRes.StoredChunks + snapRes.DedupChunks,
		WallclockUploadMs:   time.Since(t0).Milliseconds(),
	}, nil
}

// ingestFile is a small wrapper around ingest.Ingester.Ingest that
// handles file open + size discovery and forwards the resulting
// ManifestKey from the Result. The Ingester writes the manifest blob
// internally — callers receive the content key directly.
func ingestFile(
	ctx context.Context,
	ing ingest.Ingester,
	path string,
	holes []codec.HoleExtent,
	label string,
	logf func(string, ...any),
) (store.ContentKey, *ingest.Result, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f, err := os.Open(path)
	if err != nil {
		return store.ContentKey{}, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return store.ContentKey{}, nil, err
	}
	size := uint64(st.Size())

	// Effective data = file size minus hole bytes; ingest only chunks
	// data segments, so OnProgress.processed runs 0 -> effective. Using
	// it as the denominator makes % and rate reflect real work, not the
	// (often far larger) sparse logical size.
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
		dt := now.Sub(lastT).Seconds()
		rate := float64(processed-lastProcessed) / dt / mib
		pct := uint64(0)
		if effective > 0 {
			pct = min(processed*100/effective, 100)
		}
		logf("upload: %s %d/%d MiB (%d%%) %.0f MiB/s",
			label, processed/mib, effective/mib, pct, rate)
		lastT = now
		lastProcessed = processed
	}

	res, err := ing.Ingest(ctx, f, size, ingest.IngestOption{
		Holes:      holes,
		OnProgress: onProgress,
	})
	if err != nil {
		return store.ContentKey{}, nil, err
	}
	elapsed := time.Since(start)
	avg := float64(effective) / elapsed.Seconds() / mib
	logf("upload: %s ingest profile: %d MiB data in %.1fs = %.0f MiB/s (stored=%d dedup=%d)",
		label, effective/mib, elapsed.Seconds(), avg, res.StoredChunks, res.DedupChunks)
	return res.ManifestKey, res, nil
}

// HexKey hex-encodes a ContentKey for use in `manifest://<hex>` URIs.
func HexKey(k store.ContentKey) string {
	return hex.EncodeToString(k[:])
}

// SparseHoles scans an existing file for SEEK_DATA / SEEK_HOLE extents
// and returns a sorted, page-aligned list of holes restricted to the
// region [0, limit). Used by the upload path to feed the snapshot's
// memory-section sparseness into ingest.
func SparseHoles(path string, limit uint64) ([]codec.HoleExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const seekData = 3
	const seekHole = 4
	var holes []codec.HoleExtent
	off := int64(0)
	end := int64(limit)
	for off < end {
		dataOff, derr := f.Seek(off, seekData)
		if derr != nil {
			if e, ok := derr.(*os.PathError); ok && e.Err.Error() == "no such device or address" {
				if uint64(off) < limit {
					holes = append(holes, codec.HoleExtent{
						Offset: uint64(off),
						Size:   limit - uint64(off),
					})
				}
				break
			}
			return nil, derr
		}
		if dataOff >= end {
			if uint64(off) < limit {
				holes = append(holes, codec.HoleExtent{
					Offset: uint64(off),
					Size:   limit - uint64(off),
				})
			}
			break
		}
		if dataOff > off {
			holes = append(holes, codec.HoleExtent{
				Offset: uint64(off),
				Size:   uint64(dataOff - off),
			})
		}
		holeOff, herr := f.Seek(dataOff, seekHole)
		if herr != nil {
			return nil, herr
		}
		if holeOff > end {
			holeOff = end
		}
		off = holeOff
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return holes, nil
}
