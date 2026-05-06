package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// CgroupController applies CPU and memory limits to the current process
// (and inherited children) via cgroup v2.
type CgroupController struct {
	Path string // /sys/fs/cgroup/sandbox-<sid>
}

// SetupCgroup creates a cgroup v2 directory under /sys/fs/cgroup,
// writes cpu.max and memory.max, and adds the current process to it.
// Children inherit (CH spawned later joins automatically).
//
// On hosts where /sys/fs/cgroup is not cgroup v2 (or where the process
// lacks permission), SetupCgroup returns an error; the caller may
// downgrade to "no resource control" mode for dev environments.
func SetupCgroup(sandboxID string, cpuQuota float64, memMiB int) (*CgroupController, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("cgroup: empty sandbox id")
	}
	cgRoot := "/sys/fs/cgroup"
	cgPath := filepath.Join(cgRoot, "sandbox-"+sandboxID)
	if err := os.MkdirAll(cgPath, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: mkdir %s: %w", cgPath, err)
	}

	// cpu.max: "<quota> <period>" (us)
	const period = 100000 // 100ms
	quotaUs := int(cpuQuota * float64(period))
	if quotaUs < 1000 {
		quotaUs = 1000 // minimum 1ms / 100ms
	}
	if err := writeCgFile(cgPath, "cpu.max", fmt.Sprintf("%d %d", quotaUs, period)); err != nil {
		// CPU controller may not be enabled; warn-and-continue is acceptable
		// for dev environments. Caller logs.
		return nil, fmt.Errorf("cgroup: cpu.max: %w", err)
	}

	if memMiB > 0 {
		mem := strconv.Itoa(memMiB << 20)
		if err := writeCgFile(cgPath, "memory.max", mem); err != nil {
			return nil, fmt.Errorf("cgroup: memory.max: %w", err)
		}
	}

	if err := writeCgFile(cgPath, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return nil, fmt.Errorf("cgroup: add self pid: %w", err)
	}
	return &CgroupController{Path: cgPath}, nil
}

func writeCgFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Cleanup removes the cgroup directory. Must be called after all
// processes inside have exited; otherwise the rmdir fails with EBUSY.
func (c *CgroupController) Cleanup() error {
	if c == nil || c.Path == "" {
		return nil
	}
	// Move ourselves back to the root cgroup so the directory is empty.
	rootProcs := "/sys/fs/cgroup/cgroup.procs"
	_ = os.WriteFile(rootProcs, []byte(strconv.Itoa(os.Getpid())), 0o644)
	return os.Remove(c.Path)
}
