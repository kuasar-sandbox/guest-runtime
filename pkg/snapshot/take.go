// Package snapshot implements the sandbox-ctl snapshot path: CH
// /vm.snapshot orchestration, sparse memfd copy, ZIP-at-end bundle
// composition, and manifest-store upload.
//
// The ctl.sock wire protocol + listener that carries snapshot_request
// from `sandbox-ctl snapshot` to the run process lives in
// pkg/ctl (shared with the exec path).
//
// See sandbox.md §6 for the file format and timing.
package snapshot

import (
	"bytes"
	"context"
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

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/chapi"
)

// Quiescer abstracts the vhost backend's pause/resume hooks. A typical
// caller passes a callback that calls srv0.Quiesce(); srv1.Quiesce()
// and the inverse for Resume.
type Quiescer interface {
	Quiesce()
	Resume()
}

// Sources gathers the inputs Take needs.
//
// SnapshotCfg is rendered late (Take calls it with the final overlay.base
// ref — file://<sha>.overlay or manifest://<key>) because that ref depends
// on how the overlay was absorbed by the sink.
type Sources struct {
	SandboxID  string // <sid> for output filename / symlink
	APISock    string // CH api socket
	MemfdFD    int    // memfd backing the zone (read-only here; CH is paused)
	MemfdSize  int64  // ramSize
	DiffPath   string // blk1 diff file (sparse ext4)
	OwnedDiff  bool   // true iff the diff is the sandbox's own (auto-created) → eligible for zero-copy move
	StagingDir string // CH /vm.snapshot dest for config.json/state.json (caller creates+removes)

	// CHApiDeadline bounds each CH API call (pause/snapshot/resume); 0 = no
	// forced. From config.SandboxConfig.CHApiDeadline() (timeouts.ch_api).
	CHApiDeadline time.Duration

	// MergeBase{Snapshot,Overlay}: parent LOCAL files this run was restored from
	// (both set, or neither). When set, Take flattens this run's resident delta
	// ONTO them (top wins) and absorbs the MERGED result as the new top layer —
	// replacing the next-newest local layer instead of stacking (docs §3.5). The
	// caller must pair this with a snapshot.cfg whose from_refs/base_from_refs
	// DROP the parent ref. Incompatible with the zero-copy overlay move (Take
	// falls back to copy+merge). Empty ⇒ no merge (stack via the parent ref).
	MergeBaseSnapshot string // parent <sha>.snapshot abs path; memory section = [0,MemfdSize)
	MergeBaseOverlay  string // parent <sha>.overlay abs path

	// SnapshotCfg renders snapshot.cfg given the final overlay.base ref.
	SnapshotCfg func(overlayRef string) ([]byte, error)

	Quiescer Quiescer
	Logf     func(string, ...any)
}

// Outputs describes what was produced. Refs are scheme-tagged
// (file://<sha>.ext | manifest://<key>); Path is the local file (file mode
// only, "" for upload). The handler maps these into the ctl.Response.
type Outputs struct {
	OverlayRef   string
	OverlayPath  string
	SnapshotRef  string
	SnapshotPath string

	MemorySize       uint64
	MemoryResident   uint64
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Take runs the snapshot sequence (§6.2 T2-T8) and streams the two large
// artifacts (blk1 overlay, memory+ZIP bundle) through the sink — never staging
// them in the /run tmpfs. Only CH's small config.json/state.json land in
// StagingDir. The overlay is absorbed first so snapshot.cfg can carry its final
// overlay.base ref.
//
// Caller responsibility: create/remove StagingDir; supply the sink
// (fileSink for --output, ingestSink for --upload) and the SnapshotCfg builder.
func Take(s Sources, sink SnapshotSink, resumeAfter bool) (*Outputs, error) {
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
	if sink == nil {
		return nil, fmt.Errorf("snapshot: nil sink")
	}
	ctx := context.Background()
	out := &Outputs{MemorySize: uint64(s.MemfdSize)}
	ch := chapi.Client{Sock: s.APISock, RespDeadline: s.CHApiDeadline}

	// T2a: pause CH.
	pauseStart := time.Now()
	if err := ch.Pause(); err != nil {
		return nil, fmt.Errorf("CH pause: %w", err)
	}
	pausedAt := time.Now()
	resumed := false
	defer func() {
		// resume_after=false (destroy mode) is handled by the caller via
		// /vm.shutdown after Take returns; here we only resume on the
		// resume_after=true path.
		if !resumed && resumeAfter {
			_ = ch.Resume()
		}
	}()
	defer s.Quiescer.Resume() // unconditional

	// T2b: quiesce backends (steady state before the dump).
	s.Quiescer.Quiesce()

	// T3: CH /vm.snapshot → staging dir. CH writes only config.json + state.json
	// there (small); the multi-GiB memory + disk never touch the staging tmpfs —
	// they stream straight to the sink.
	dumpStart := time.Now()
	if err := ch.Snapshot("file://" + s.StagingDir); err != nil {
		return nil, fmt.Errorf("CH snapshot: %w", err)
	}
	configJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config.json: %w", err)
	}
	stateJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read state.json: %w", err)
	}

	// T4: overlay → sink. Done first so snapshot.cfg below carries the final
	// overlay.base ref.
	//
	// Fast path: when the diff is the sandbox's own and the sandbox is being
	// destroyed (no resume), the diff is consumed — hand it to the sink's
	// OverlayMover to rename (zero-copy) instead of sparse-copying multi-GiB.
	// Falls back to the streaming copy on cross-fs rename or any other sink.
	// merging: the sandbox was restored from a LOCAL snapshot; flatten this run's
	// resident delta onto the parent local layer (replace, not stack — §3.5).
	merging := s.MergeBaseSnapshot != "" && s.MergeBaseOverlay != ""
	if mover, ok := sink.(OverlayMover); ok && s.OwnedDiff && !resumeAfter && !merging {
		ref, path, mErr := mover.MoveOverlay(s.DiffPath)
		if mErr == nil {
			out.OverlayRef, out.OverlayPath = ref, path
		} else {
			logf("snapshot: overlay move fell back to copy: %v", mErr)
		}
	}
	if out.OverlayRef == "" { // not moved (no mover, not owned, resume, merging, or move failed)
		diff, err := os.Open(s.DiffPath)
		if err != nil {
			return nil, fmt.Errorf("open blk1.diff: %w", err)
		}
		defer diff.Close()
		dstat, err := diff.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat blk1.diff: %w", err)
		}
		overlayHoles, err := walkHolesCodec(int(diff.Fd()), dstat.Size())
		if err != nil {
			return nil, fmt.Errorf("overlay holes: %w", err)
		}
		var src io.ReadSeeker = diff
		holes := overlayHoles
		if merging {
			base, baseHoles, berr := openMergeBase(s.MergeBaseOverlay, dstat.Size())
			if berr != nil {
				return nil, fmt.Errorf("merge overlay base: %w", berr)
			}
			defer base.Close()
			src, holes = mergeSparse(diff, overlayHoles, base, baseHoles, dstat.Size())
		}
		out.OverlayRef, out.OverlayPath, err = sink.AbsorbOverlay(ctx, src, holes)
		if err != nil {
			return nil, fmt.Errorf("absorb overlay: %w", err)
		}
	}

	// T5: snapshot.cfg (final overlay ref) → ZIP trailer.
	snapshotCfg, err := s.SnapshotCfg(out.OverlayRef)
	if err != nil {
		return nil, fmt.Errorf("build snapshot.cfg: %w", err)
	}
	zipBytes, err := BuildZIP(map[string][]byte{
		"config.json":  configJSON,
		"state.json":   stateJSON,
		"snapshot.cfg": snapshotCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("build zip: %w", err)
	}

	// T6: [memory][ZIP] bundle → sink, streamed from the memfd (CH paused, so
	// the mapping is stable); only resident pages are read/transferred.
	memHoles, err := walkHolesCodec(s.MemfdFD, s.MemfdSize)
	if err != nil {
		return nil, fmt.Errorf("memory holes: %w", err)
	}
	var memSrc io.ReadSeeker = memfdReader(s.MemfdFD, s.MemfdSize)
	memSrcHoles := memHoles
	if merging {
		base, baseHoles, berr := openMergeBase(s.MergeBaseSnapshot, s.MemfdSize)
		if berr != nil {
			return nil, fmt.Errorf("merge memory base: %w", berr)
		}
		defer base.Close()
		memSrc, memSrcHoles = mergeSparse(memSrc, memHoles, base, baseHoles, s.MemfdSize)
	}
	out.MemoryResident = residentBytes(s.MemfdSize, memSrcHoles) // bytes actually written (merged)
	out.SnapshotRef, out.SnapshotPath, err = sink.AbsorbBundle(
		ctx, memSrc, memSrcHoles, bytes.NewReader(zipBytes))
	if err != nil {
		return nil, fmt.Errorf("absorb bundle: %w", err)
	}
	dumpEnd := time.Now()

	// T8: resume (destroy path handled by caller).
	if resumeAfter {
		if err := ch.Resume(); err != nil {
			return nil, fmt.Errorf("CH resume: %w", err)
		}
		resumed = true
	}

	out.WallclockPauseMs = pausedAt.Sub(pauseStart).Milliseconds()
	out.WallclockDumpMs = dumpEnd.Sub(dumpStart).Milliseconds()
	logf("snapshot: overlay=%s snapshot=%s memory_resident=%d", out.OverlayRef, out.SnapshotRef, out.MemoryResident)
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
