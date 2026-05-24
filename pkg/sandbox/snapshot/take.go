// Package snapshot implements the sandbox-ctl snapshot path: CH
// /vm.snapshot orchestration, sparse memfd copy, ZIP-at-end bundle
// composition, and manifest-store upload.
//
// The ctl.sock wire protocol + listener that carries snapshot_request
// from `sandbox-ctl snapshot` to the run process lives in
// pkg/sandbox/ctl (shared with the exec path).
//
// See sandbox.md §6 for the file format and timing.
package snapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Quiescer abstracts the vhost backend's pause/resume hooks. A typical
// caller passes a callback that calls srv0.Quiesce(); srv1.Quiesce()
// and the inverse for Resume.
type Quiescer interface {
	Quiesce()
	Resume()
}

// Sources gathers the file inputs Take needs.
//
// SnapshotCfg is the YAML-encoded snapshot.cfg (see docs/sandbox.md §3.4)
// that the caller pre-renders with capacity, runtime_ref, base_ref;
// overlay.base is filled in by Take before writing to ZIP because it
// depends on the overlay digest computed during T4.
type Sources struct {
	SandboxID  string // <sid> for output filename
	APISock    string // CH api socket
	MemfdFD    int    // memfd backing the zone
	MemfdSize  int64  // ramSize
	DiffPath   string // blk1 diff file (sparse ext4)
	StagingDir string // CH /vm.snapshot dest (caller creates+removes)

	// SnapshotCfg is a function: given the final overlay.base value
	// (file://<digest>.overlay or manifest://<key>), returns the
	// rendered YAML bytes. Defers building the cfg until overlay's
	// digest is known.
	SnapshotCfg func(overlayRef string) ([]byte, error)

	Quiescer Quiescer
	Logf     func(string, ...any)
}

// Outputs describes what was written.
//
//   - SnapshotPath is the local <out_dir>/<sid>.snapshot
//   - OverlayPath is the local <out_dir>/<sha256>.overlay
//     (only set in --output mode; --upload skips this and uses Upload())
//   - OverlaySha256 is the hex-encoded SHA256 of the overlay (also embedded
//     in OverlayPath as basename)
type Outputs struct {
	SnapshotPath   string
	SnapshotSha256 string // --output mode: content digest naming <sha256>.snapshot
	OverlayPath    string
	OverlaySha256  string

	MemorySize       uint64
	MemoryResident   uint64
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Take runs the snapshot sequence (§6.2 T2-T8).
//
// Caller responsibility:
//   - Set up StagingDir; remove on cleanup
//   - Pre-build a SnapshotCfg builder (the function takes the final overlay_ref)
//   - For --output mode pass outDir; for --upload caller passes empty outDir
//     and uses Upload() afterward to ingest the staged <sid>.snapshot
func Take(s Sources, outDir string, resumeAfter bool) (*Outputs, error) {
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if s.SandboxID == "" {
		return nil, fmt.Errorf("snapshot: empty SandboxID")
	}
	if s.SnapshotCfg == nil {
		return nil, fmt.Errorf("snapshot: nil SnapshotCfg builder")
	}
	out := &Outputs{
		MemorySize: uint64(s.MemfdSize),
	}

	// T2a: pause CH
	pauseStart := time.Now()
	if err := CHPause(s.APISock); err != nil {
		return nil, fmt.Errorf("CH pause: %w", err)
	}
	pausedAt := time.Now()
	resumed := false
	defer func() {
		// resume_after=false (destroy mode) is handled by caller via /vm.shutdown
		// after Take returns, so here we only resume on the resume_after=true path
		if !resumed && resumeAfter {
			_ = CHResume(s.APISock)
		}
	}()
	defer s.Quiescer.Resume() // unconditional

	// T2b: quiesce backends
	s.Quiescer.Quiesce()

	// T3: CH /vm.snapshot → staging dir
	dumpStart := time.Now()
	if err := CHSnapshot(s.APISock, "file://"+s.StagingDir); err != nil {
		return nil, fmt.Errorf("CH snapshot: %w", err)
	}

	// Read config.json + state.json into memory.
	configJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config.json: %w", err)
	}
	stateJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read state.json: %w", err)
	}

	if outDir == "" {
		// --upload mode: produce <sid>.snapshot in StagingDir; overlay
		// is not materialized as a file (Upload() ingests blk1.diff
		// directly from DiffPath).
		outDir = s.StagingDir
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir out: %w", err)
	}

	// T4a: stream-hash blk1.diff → digest. Then T4b: sparse copy to
	// <out_dir>/<digest>.overlay.
	overlayRef := ""
	if outDir != s.StagingDir {
		// --output mode: materialize overlay locally with sha256 name.
		digest, err := hashSparseFile(s.DiffPath)
		if err != nil {
			return nil, fmt.Errorf("hash blk1.diff: %w", err)
		}
		out.OverlaySha256 = digest
		out.OverlayPath = filepath.Join(outDir, digest+".overlay")
		if err := sparseCopyFile(s.DiffPath, out.OverlayPath); err != nil {
			return nil, fmt.Errorf("sparse copy overlay: %w", err)
		}
		overlayRef = "file://" + digest + ".overlay"
		logf("snapshot: %s.overlay written", digest[:12])
	}
	// --upload path: caller's Upload() ingests blk1.diff and patches
	// snapshot.cfg's overlay.base = manifest://<key>; we still need a
	// placeholder here so the ZIP can be built. The caller will rewrite
	// overlay.base post-upload before ingesting <sid>.snapshot.
	if overlayRef == "" {
		overlayRef = "file://placeholder.overlay" // patched by Upload()
	}

	// T5: build snapshot.cfg with overlay_ref filled in.
	snapshotCfg, err := s.SnapshotCfg(overlayRef)
	if err != nil {
		return nil, fmt.Errorf("build snapshot.cfg: %w", err)
	}

	// T6: sparse copy memory + ZIP append. Write to a working path first; in
	// --output mode finalize to <sha256>.snapshot + a <sid>.snapshot symlink so
	// file-mode from_refs chains reference an immutable, content-addressed name
	// (docs/sandbox.md §6.1). --upload writes <sid>.snapshot in StagingDir and
	// Upload() ingests it (named by manifest key), so no rename there.
	isOutput := outDir != s.StagingDir
	workPath := filepath.Join(outDir, s.SandboxID+".snapshot")
	if isOutput {
		workPath = filepath.Join(outDir, s.SandboxID+".snapshot.partial")
	}
	snapOut, err := os.Create(workPath)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	memCopied, err := SparseCopy(snapOut, s.MemfdFD, s.MemfdSize)
	if err != nil {
		snapOut.Close()
		return nil, fmt.Errorf("sparse copy memory: %w", err)
	}
	out.MemoryResident = uint64(memCopied)

	// T6c: append ZIP at offset = memfdSize.
	if _, err := AppendZIP(snapOut, s.MemfdSize, map[string][]byte{
		"config.json":  configJSON,
		"state.json":   stateJSON,
		"snapshot.cfg": snapshotCfg,
	}); err != nil {
		snapOut.Close()
		return nil, fmt.Errorf("append ZIP: %w", err)
	}
	if err := snapOut.Sync(); err != nil {
		snapOut.Close()
		return nil, fmt.Errorf("fsync snapshot: %w", err)
	}
	snapOut.Close()
	out.SnapshotPath = workPath

	if isOutput {
		// Content-addressed name (skip-holes digest) + <sid>.snapshot symlink.
		digest, err := hashSparseFile(workPath)
		if err != nil {
			return nil, fmt.Errorf("hash snapshot: %w", err)
		}
		finalPath := filepath.Join(outDir, digest+".snapshot")
		if err := os.Rename(workPath, finalPath); err != nil {
			return nil, fmt.Errorf("rename snapshot: %w", err)
		}
		linkPath := filepath.Join(outDir, s.SandboxID+".snapshot")
		_ = os.Remove(linkPath)
		if err := os.Symlink(digest+".snapshot", linkPath); err != nil {
			return nil, fmt.Errorf("symlink %s.snapshot: %w", s.SandboxID, err)
		}
		out.SnapshotPath = finalPath
		out.SnapshotSha256 = digest
		logf("snapshot: %s.snapshot → %s.snapshot, memory_resident=%d", s.SandboxID, digest[:12], memCopied)
	} else {
		logf("snapshot: %s.snapshot written, memory_resident=%d", s.SandboxID, memCopied)
	}
	dumpEnd := time.Now()

	// T8: resume (only when caller asked; destroy path handled by caller)
	if resumeAfter {
		if err := CHResume(s.APISock); err != nil {
			return nil, fmt.Errorf("CH resume: %w", err)
		}
		resumed = true
	}

	out.WallclockPauseMs = pausedAt.Sub(pauseStart).Milliseconds()
	out.WallclockDumpMs = dumpEnd.Sub(dumpStart).Milliseconds()
	return out, nil
}

// hashSparseFile computes a content digest over the data extents of path,
// skipping holes: each data extent's (offset, length) framing plus its bytes
// are folded into SHA256, and the file's logical size is folded at the end.
// This is fast (reads only resident data, not the zero pages of a multi-GiB
// sparse image) and layout-sensitive (different hole distributions → different
// digest). It is NOT equal to the SHA256 of the full logical (hole=0) byte
// stream — sparse/dense representation invariance is intentionally traded for
// speed; the snapshot pipeline only ever emits canonical-sparse files. See
// docs/sandbox.md §6.1.
func hashSparseFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	fd := int(f.Fd())
	h := sha256.New()
	var hdr [16]byte
	var off int64
	for off < size {
		dataOff, err := syscall.Seek(fd, off, seekData)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				break // no more data; remainder is a trailing hole
			}
			return "", fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		holeOff, err := syscall.Seek(fd, dataOff, seekHole)
		if err != nil {
			return "", fmt.Errorf("SEEK_HOLE at %d: %w", dataOff, err)
		}
		if holeOff > size {
			holeOff = size
		}
		if holeOff <= dataOff {
			off = holeOff
			continue
		}
		binary.LittleEndian.PutUint64(hdr[0:8], uint64(dataOff))
		binary.LittleEndian.PutUint64(hdr[8:16], uint64(holeOff-dataOff))
		h.Write(hdr[:])
		if _, err := io.Copy(h, io.NewSectionReader(f, dataOff, holeOff-dataOff)); err != nil {
			return "", fmt.Errorf("hash data [%d,%d): %w", dataOff, holeOff, err)
		}
		off = holeOff
	}
	// Fold logical size so the trailing-hole length is part of the identity.
	binary.LittleEndian.PutUint64(hdr[0:8], uint64(size))
	h.Write(hdr[:8])
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sparseCopyFile copies src to dst preserving SEEK_DATA/SEEK_HOLE
// extents. Logical size of dst equals src.
func sparseCopyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := SparseCopy(dst, int(src.Fd()), st.Size()); err != nil {
		return err
	}
	return dst.Sync()
}
