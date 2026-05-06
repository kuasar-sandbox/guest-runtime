package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/chunker"
	"github.com/fullof-work/mass-sandbox/pkg/crypto"
	"github.com/fullof-work/mass-sandbox/pkg/ingest"
	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/store"
)

// Storer is the minimal store-ctl client surface upload needs: GetSalt
// + Put (for chunks) + a Manifest serialization + put-manifest. Mirror
// of what manifest-ctl's `store --put-manifest` short form uses.
type Storer interface {
	GetSalt(ctx context.Context) (generation string, salt [32]byte, err error)
	Put(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) (isNew bool, err error)
}

// UploadSources gathers the inputs needed to ingest disk + sandbox.snapshot.
// All paths point at files already produced by Take into a staging dir.
type UploadSources struct {
	DiskPath        string         // local disk.ext4 from Take
	SnapshotPath    string         // local sandbox.snapshot from Take (memory + ZIP at end)
	SnapshotHoles   []manifest.HoleExtent // hole extents inside SnapshotPath (memory section only)
	CustomerKey     [32]byte
	ChunkConfig     chunker.Config
	ChunkEncryptor  crypto.ChunkEncryptor
	KTEncryptor     crypto.KeyTableEncryptor
	Storer          Storer
	Logf            func(string, ...any)
}

// UploadResult summarises the bytes/dedup metrics for both ingests.
type UploadResult struct {
	DiskKey            store.ContentKey
	SnapshotKey        store.ContentKey
	DiskStoredBytes    uint64
	DiskDedupChunks    uint32
	DiskTotalChunks    uint32
	SnapshotStoredBytes uint64
	SnapshotDedupChunks uint32
	SnapshotTotalChunks uint32
	WallclockUploadMs  int64
}

// Upload ingests disk.ext4 then rewrites the sandbox.cfg embedded in
// sandbox.snapshot to point at the disk's manifest key, then ingests
// sandbox.snapshot. Returns the snapshot manifest key (= the `--snapshot
// manifest://<key>` reference for restore).
//
// The disk + snapshot must reside in the same target store (single
// `--upload` switch in CLI). Two-step ingest order matters:
//  1. Disk ingest first → disk_key known.
//  2. Rewrite sandbox.cfg's overlay.base = "manifest://<disk-key>"
//     (the sandbox.snapshot ZIP at the end already contains a
//     placeholder; we patch it in-place via re-packaging the ZIP
//     before snapshot ingest).
//  3. Snapshot ingest with hole map preserved.
func Upload(ctx context.Context, src UploadSources) (*UploadResult, error) {
	logf := src.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t0 := time.Now()

	// Salt + ingester (shared by both ingests).
	gen, salt, err := src.Storer.GetSalt(ctx)
	if err != nil {
		return nil, fmt.Errorf("upload: GetSalt: %w", err)
	}
	logf("upload: store generation=%s", gen)
	ing := ingest.NewIngester(src.Storer.Put, src.ChunkEncryptor, src.KTEncryptor)

	// 1. Ingest disk.ext4 (fresh from sparse copy).
	diskKey, diskRes, err := ingestFile(ctx, ing, src.DiskPath, src.CustomerKey, salt,
		src.ChunkConfig, nil /*holes; sparse copy preserved them as fs holes which Ingest can detect via SEEK_DATA but we pass nil and rely on chunker; for cleanliness we skip hole tracking here and let dedup take care*/, src.Storer.Put)
	if err != nil {
		return nil, fmt.Errorf("upload disk: %w", err)
	}
	logf("upload: disk ingested key=%x stored=%d dedup=%d total=%d",
		diskKey, diskRes.StoredChunks, diskRes.DedupChunks, diskRes.StoredChunks+diskRes.DedupChunks)

	// 2. Patch sandbox.cfg inside sandbox.snapshot's trailing ZIP so
	//    overlay.base references manifest://<diskKey>. The patch is a
	//    "read ZIP entry → JSON edit → re-encode trailing ZIP". The
	//    memory section stays untouched; the sparse layout is preserved.
	if err := rewriteSandboxCfgInBundle(src.SnapshotPath, diskKey); err != nil {
		return nil, fmt.Errorf("upload: rewrite sandbox.cfg: %w", err)
	}
	logf("upload: sandbox.cfg in bundle patched: overlay.base=manifest://%x", diskKey)

	// 3. Ingest sandbox.snapshot with hole map (memory section's holes).
	snapKey, snapRes, err := ingestFile(ctx, ing, src.SnapshotPath, src.CustomerKey, salt,
		src.ChunkConfig, src.SnapshotHoles, src.Storer.Put)
	if err != nil {
		return nil, fmt.Errorf("upload snapshot: %w", err)
	}
	logf("upload: snapshot ingested key=%x stored=%d dedup=%d total=%d",
		snapKey, snapRes.StoredChunks, snapRes.DedupChunks, snapRes.StoredChunks+snapRes.DedupChunks)

	return &UploadResult{
		DiskKey:             diskKey,
		SnapshotKey:         snapKey,
		DiskStoredBytes:     diskRes.StoredBytes,
		DiskDedupChunks:     diskRes.DedupChunks,
		DiskTotalChunks:     diskRes.StoredChunks + diskRes.DedupChunks,
		SnapshotStoredBytes: snapRes.StoredBytes,
		SnapshotDedupChunks: snapRes.DedupChunks,
		SnapshotTotalChunks: snapRes.StoredChunks + snapRes.DedupChunks,
		WallclockUploadMs:   time.Since(t0).Milliseconds(),
	}, nil
}

// ingestFile is a small wrapper around ingest.Ingest that handles file
// open + size + manifest seal + put-manifest (= ContentKey of manifest
// bytes written to PartitionManifest). Returns the manifest key.
func ingestFile(
	ctx context.Context,
	ing *ingest.Ingester,
	path string,
	customerKey [32]byte,
	salt [32]byte,
	chunkCfg chunker.Config,
	holes []manifest.HoleExtent,
	put func(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) (bool, error),
) (store.ContentKey, *ingest.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return store.ContentKey{}, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return store.ContentKey{}, nil, err
	}

	res, err := ing.Ingest(ctx, f, uint64(st.Size()), ingest.Config{
		CustomerKey: customerKey,
		Salt:        salt,
		ChunkConfig: chunkCfg,
		Holes:       holes,
	})
	if err != nil {
		return store.ContentKey{}, nil, err
	}
	// Marshal manifest blob and put it under the manifest partition.
	body, err := manifest.Marshal(res.Manifest, res.SealedKeyTable)
	if err != nil {
		return store.ContentKey{}, nil, fmt.Errorf("marshal manifest: %w", err)
	}
	mkey := contentKey(body)
	if _, err := put(ctx, store.PartitionManifest, mkey, body); err != nil {
		return store.ContentKey{}, nil, fmt.Errorf("put manifest: %w", err)
	}
	return mkey, res, nil
}

func contentKey(b []byte) store.ContentKey {
	var k store.ContentKey
	h := hashSHA256(b)
	copy(k[:], h[:])
	return k
}

// HexKey hex-encodes a ContentKey for use in `manifest://<hex>` URIs.
func HexKey(k store.ContentKey) string {
	return hex.EncodeToString(k[:])
}

// hashSHA256 returns the SHA-256 of b.
func hashSHA256(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// SparseHoles scans an existing file for SEEK_DATA / SEEK_HOLE extents
// and returns a sorted, page-aligned list of holes restricted to the
// region [0, limit). Used by the upload path to feed the snapshot's
// memory-section sparseness into ingest.
func SparseHoles(path string, limit uint64) ([]manifest.HoleExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const seekData = 3
	const seekHole = 4
	var holes []manifest.HoleExtent
	off := int64(0)
	end := int64(limit)
	for off < end {
		dataOff, derr := f.Seek(off, seekData)
		if derr != nil {
			// ENXIO → no more data: rest is a hole until end.
			if e, ok := derr.(*os.PathError); ok && e.Err.Error() == "no such device or address" {
				if uint64(off) < limit {
					holes = append(holes, manifest.HoleExtent{
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
				holes = append(holes, manifest.HoleExtent{
					Offset: uint64(off),
					Size:   limit - uint64(off),
				})
			}
			break
		}
		if dataOff > off {
			holes = append(holes, manifest.HoleExtent{
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
