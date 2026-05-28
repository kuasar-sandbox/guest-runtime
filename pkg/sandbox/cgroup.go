package sandbox

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// CgroupConfig describes the cgroup join target for a sandbox.
//
// Path must point at an EXISTING cgroup directory (e.g. created by an
// orchestrator, systemd unit, or operator script). sandbox-ctl never
// creates the directory and never rmdir on exit — ownership is
// external. Empty Path means "no cgroup operations" (no-cgroup mode in
// docs/sandbox.md §4.1).
//
// MemoryMaxBytes / MemoryHighBytes are written verbatim to memory.max /
// memory.high. The caller computes them from
//
//	MemoryMaxBytes  = capacity.memory + overhead.memory
//	MemoryHighBytes = watermark_high.memory   (default allocatable * 0.875)
//
// CPUMaxQuotaUs is the cpu.max quota; period is fixed 100000us. Set to
//
//	CPUMaxQuotaUs = capacity.cpu * 100000
//
// so the cgroup can burst to capacity when uncontended; cpu.weight does
// the floor enforcement under contention.
//
// CPUWeight is clamp(round(allocatable.cpu * 100), 1, 10000); see
// docs/sandbox.md §9.2.
type CgroupConfig struct {
	Path            string
	MemoryMaxBytes  uint64
	MemoryHighBytes uint64
	CPUMaxQuotaUs   int
	CPUWeight       uint64
}

// CgroupController tracks whether sandbox-ctl successfully joined a
// cgroup. Cleanup uses this to decide whether to move the PID back to
// the root cgroup.
type CgroupController struct {
	Path   string
	joined bool
}

// JoinCgroup writes resource limits to an existing cgroup and adds the
// current process. Children inherit (CH spawned later joins
// automatically).
//
// Returns a controller with Path set and joined=true on success. When
// cfg.Path is empty the function is a no-op and returns a zero-value
// controller (Cleanup is also a no-op).
//
// The cgroup directory must exist. Failures (path missing, permission
// denied, controller not enabled in subtree_control) are propagated.
// memory.swap.max is best-effort — some kernels lack the swap controller
// and we warn rather than fail.
func JoinCgroup(cfg CgroupConfig) (*CgroupController, error) {
	if cfg.Path == "" {
		return &CgroupController{}, nil
	}

	st, err := os.Stat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("cgroup: stat %q: %w", cfg.Path, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("cgroup: %q is not a directory", cfg.Path)
	}

	if err := writeCgFile(cfg.Path, "memory.max", strconv.FormatUint(cfg.MemoryMaxBytes, 10)); err != nil {
		return nil, fmt.Errorf("cgroup: memory.max: %w", err)
	}
	if cfg.MemoryHighBytes > 0 {
		if err := writeCgFile(cfg.Path, "memory.high", strconv.FormatUint(cfg.MemoryHighBytes, 10)); err != nil {
			return nil, fmt.Errorf("cgroup: memory.high: %w", err)
		}
	}
	// Disable swap so cgroup OOM signals are unambiguous. swap.max may
	// be unavailable on hosts compiled without the swap controller; that
	// is acceptable — we want zero swap and absence of the file means
	// effectively zero anyway.
	if err := writeCgFile(cfg.Path, "memory.swap.max", "0"); err != nil {
		log.Printf("[sandbox-ctl] cgroup: memory.swap.max write skipped: %v", err)
	}

	const period = 100000
	if err := writeCgFile(cfg.Path, "cpu.max", fmt.Sprintf("%d %d", cfg.CPUMaxQuotaUs, period)); err != nil {
		return nil, fmt.Errorf("cgroup: cpu.max: %w", err)
	}
	if cfg.CPUWeight > 0 {
		if err := writeCgFile(cfg.Path, "cpu.weight", strconv.FormatUint(cfg.CPUWeight, 10)); err != nil {
			return nil, fmt.Errorf("cgroup: cpu.weight: %w", err)
		}
	}

	if err := writeCgFile(cfg.Path, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return nil, fmt.Errorf("cgroup: add self pid: %w", err)
	}

	return &CgroupController{Path: cfg.Path, joined: true}, nil
}

func writeCgFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Cleanup moves sandbox-ctl back to the root cgroup so the per-sandbox
// cgroup is left empty for its external owner to remove. The cgroup
// directory itself is never rmdir'd — ownership is external.
//
// No-op when the controller never joined (empty Path or no-cgroup mode).
func (c *CgroupController) Cleanup() error {
	if c == nil || !c.joined {
		return nil
	}
	rootProcs := "/sys/fs/cgroup/cgroup.procs"
	_ = os.WriteFile(rootProcs, []byte(strconv.Itoa(os.Getpid())), 0o644)
	return nil
}

// JoinCgroupForConfig is the convenience entry point used by restore.Run.
// Derives a CgroupConfig from a SandboxConfig and joins, but with
// MemoryHighBytes zeroed so the boot-transient page-fault burst is not
// PSI-throttled — Settled/SettledRestore writes memory.high once the
// transient is over (Issue 4). Returns a zero-value controller (Cleanup
// no-op) when CgroupPath is unset (no-cgroup mode).
func JoinCgroupForConfig(cfg *SandboxConfig) (*CgroupController, error) {
	cgCfg, err := buildCgroupConfig(cfg)
	if err != nil {
		return nil, err
	}
	cgCfg.MemoryHighBytes = 0
	return JoinCgroup(cgCfg)
}

// buildCgroupConfig translates a SandboxConfig into a CgroupConfig.
// Returns a zero-Path config when CgroupPath is unset (no-cgroup mode); JoinCgroup
// will then no-op.
//
// Memory.max derives from capacity + overhead so guest legitimate use
// up to allocatable does not cgroup-OOM the CH process. Memory.high is
// the watermark (default allocatable * 0.875). cpu.max = capacity *
// 100000us per 100000us period; cpu.weight from allocatable.cpu.
func buildCgroupConfig(cfg *SandboxConfig) (CgroupConfig, error) {
	if cfg.Resources.Control.CgroupPath == "" {
		return CgroupConfig{}, nil
	}
	capMem, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("capacity.memory: %w", err)
	}
	overhead, err := cfg.OverheadMemoryBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("overhead.memory: %w", err)
	}
	wm, err := cfg.WatermarkHighBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("watermark_high.memory: %w", err)
	}
	return CgroupConfig{
		Path:            cfg.Resources.Control.CgroupPath,
		MemoryMaxBytes:  capMem + overhead,
		MemoryHighBytes: wm,
		CPUMaxQuotaUs:   cfg.Resources.Capacity.CPU * 100000,
		CPUWeight:       cfg.CPUWeight(),
	}, nil
}
