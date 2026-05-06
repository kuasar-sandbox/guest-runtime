package snapshot

import (
	"fmt"
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
type Sources struct {
	APISock     string // CH api socket
	MemfdFD     int    // memfd backing the zone
	MemfdSize   int64  // ramSize
	DiffPath    string // blk1 diff file (sparse ext4)
	StagingDir  string // CH /vm.snapshot dest (caller creates+removes)
	SandboxCfg  []byte // pre-rendered sandbox.cfg (caller fills disk ref)
	Quiescer    Quiescer
	Logf        func(string, ...any)
}

// Outputs describes what was written.
type Outputs struct {
	SnapshotPath string
	DiskPath     string

	MemorySize       uint64
	MemoryResident   uint64
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Take runs the snapshot sequence (§6.2 T2-T8).
//
// Caller responsibility:
//  - Set up StagingDir; remove on cleanup
//  - Compute SandboxCfg (with disk reference filled in: file:// or
//    manifest://)
//  - Pass outDir for local mode (sandbox.snapshot + disk.ext4 written
//    there). For --upload mode, pass empty outDir; Take writes
//    sandbox.snapshot to StagingDir and the caller streams it to ingest.
func Take(s Sources, outDir string, resumeAfter bool) (*Outputs, error) {
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
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
		outDir = s.StagingDir
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir out: %w", err)
	}

	// T4: sparse copy disk
	out.DiskPath = filepath.Join(outDir, "disk.ext4")
	diskOut, err := os.Create(out.DiskPath)
	if err != nil {
		return nil, fmt.Errorf("create disk.ext4: %w", err)
	}
	diskSrc, err := os.OpenFile(s.DiffPath, os.O_RDONLY, 0)
	if err != nil {
		diskOut.Close()
		return nil, fmt.Errorf("open diff: %w", err)
	}
	diskInfo, err := diskSrc.Stat()
	if err != nil {
		diskSrc.Close()
		diskOut.Close()
		return nil, fmt.Errorf("stat diff: %w", err)
	}
	diskSize := diskInfo.Size()
	if _, err := SparseCopy(diskOut, int(diskSrc.Fd()), diskSize); err != nil {
		diskSrc.Close()
		diskOut.Close()
		return nil, fmt.Errorf("sparse copy disk: %w", err)
	}
	if err := diskOut.Sync(); err != nil {
		diskSrc.Close()
		diskOut.Close()
		return nil, fmt.Errorf("fsync disk: %w", err)
	}
	diskSrc.Close()
	diskOut.Close()
	logf("snapshot: disk.ext4 written, logical=%d", diskSize)

	// T6: sparse copy memory + ZIP append
	out.SnapshotPath = filepath.Join(outDir, "sandbox.snapshot")
	snapOut, err := os.Create(out.SnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("create sandbox.snapshot: %w", err)
	}
	defer snapOut.Close()
	memCopied, err := SparseCopy(snapOut, s.MemfdFD, s.MemfdSize)
	if err != nil {
		return nil, fmt.Errorf("sparse copy memory: %w", err)
	}
	out.MemoryResident = uint64(memCopied)

	// T6c: append ZIP at offset = memfdSize
	if _, err := AppendZIP(snapOut, s.MemfdSize, map[string][]byte{
		"config.json":  configJSON,
		"state.json":   stateJSON,
		"sandbox.cfg":  s.SandboxCfg,
	}); err != nil {
		return nil, fmt.Errorf("append ZIP: %w", err)
	}
	if err := snapOut.Sync(); err != nil {
		return nil, fmt.Errorf("fsync snapshot: %w", err)
	}
	dumpEnd := time.Now()
	logf("snapshot: sandbox.snapshot written, memory_resident=%d", memCopied)

	// T8: resume
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
