package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	SnapshotPath  string
	OverlayPath   string
	OverlaySha256 string

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
		digest, err := streamHash(s.DiffPath)
		if err != nil {
			return nil, fmt.Errorf("stream-hash blk1.diff: %w", err)
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

	// T6: sparse copy memory + ZIP append → <sid>.snapshot
	out.SnapshotPath = filepath.Join(outDir, s.SandboxID+".snapshot")
	snapOut, err := os.Create(out.SnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("create %s.snapshot: %w", s.SandboxID, err)
	}
	defer snapOut.Close()
	memCopied, err := SparseCopy(snapOut, s.MemfdFD, s.MemfdSize)
	if err != nil {
		return nil, fmt.Errorf("sparse copy memory: %w", err)
	}
	out.MemoryResident = uint64(memCopied)

	// T6c: append ZIP at offset = memfdSize.
	if _, err := AppendZIP(snapOut, s.MemfdSize, map[string][]byte{
		"config.json":  configJSON,
		"state.json":   stateJSON,
		"snapshot.cfg": snapshotCfg,
	}); err != nil {
		return nil, fmt.Errorf("append ZIP: %w", err)
	}
	if err := snapOut.Sync(); err != nil {
		return nil, fmt.Errorf("fsync snapshot: %w", err)
	}
	dumpEnd := time.Now()
	logf("snapshot: %s.snapshot written, memory_resident=%d", s.SandboxID, memCopied)

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

// streamHash returns hex(SHA256(file bytes including sparse holes as 0)).
// Reads sequentially with default OS buffering — kernel zero-fills holes
// so the resulting hash is invariant to sparse representation (two
// dense/sparse copies of the same logical content hash identically).
func streamHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
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
