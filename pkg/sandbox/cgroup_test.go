package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJoinCgroup_EmptyPath_NoOp(t *testing.T) {
	cg, err := JoinCgroup(CgroupConfig{Path: ""})
	if err != nil {
		t.Fatalf("JoinCgroup with empty path: %v", err)
	}
	if cg == nil {
		t.Fatal("expected non-nil controller, got nil")
	}
	if cg.Path != "" {
		t.Errorf("Path = %q, want empty", cg.Path)
	}
	if cg.Active() {
		t.Error("Active() = true, want false (no-cgroup mode)")
	}
	// Cleanup must also be a no-op (no panic, no error).
	if err := cg.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
}

func TestJoinCgroup_PathMissing(t *testing.T) {
	_, err := JoinCgroup(CgroupConfig{Path: "/this/path/does/not/exist/abc"})
	if err == nil {
		t.Fatal("expected error for non-existent path, got nil")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("error %q does not mention stat", err.Error())
	}
}

func TestJoinCgroup_PathIsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := JoinCgroup(CgroupConfig{Path: f})
	if err == nil {
		t.Fatal("expected error for non-directory path, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q does not mention 'not a directory'", err.Error())
	}
}

// TestJoinCgroupForConfig_DefersMemoryHigh verifies the regression fix
// for Issue 4: the convenience entry point used by restore.Run zeroes
// MemoryHighBytes so the boot/replay transient page-fault burst is not
// PSI-throttled. The actual memory.high write is deferred to
// SettledRestore.
func TestJoinCgroupForConfig_DefersMemoryHigh(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"memory.max", "memory.swap.max", "cpu.max", "cpu.weight", "cgroup.procs"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := makeMinimalCfg()
	cfg.Resources.Control.CgroupPath = dir

	if _, err := JoinCgroupForConfig(cfg); err != nil {
		t.Fatalf("JoinCgroupForConfig: %v", err)
	}
	// memory.high file must NOT have been created. (We didn't pre-create
	// it, and JoinCgroup would error if it tried to write a missing file.)
	if _, err := os.Stat(filepath.Join(dir, "memory.high")); err == nil {
		t.Error("memory.high was written; JoinCgroupForConfig should defer it")
	}
}

func TestBuildCgroupConfig_ModeA(t *testing.T) {
	cfg := makeMinimalCfg()
	// makeMinimalCfg leaves CgroupPath empty.
	got, err := buildCgroupConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "" {
		t.Errorf("no-cgroup mode should yield empty Path, got %q", got.Path)
	}
}

func TestBuildCgroupConfig_ModeB(t *testing.T) {
	dir := t.TempDir()
	cfg := makeMinimalCfg()
	cfg.Resources.Control.CgroupPath = dir
	// capacity=4GiB, allocatable.memory=2GiB, allocatable.cpu=1.5
	got, err := buildCgroupConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dir {
		t.Errorf("Path = %q, want %q", got.Path, dir)
	}
	// memory.max = capacity + overhead (default 32 MiB)
	wantMax := uint64(4<<30) + (32 << 20)
	if got.MemoryMaxBytes != wantMax {
		t.Errorf("MemoryMaxBytes = %d, want %d", got.MemoryMaxBytes, wantMax)
	}
	// memory.high default = allocatable * 0.875
	wantHigh := uint64(float64(2<<30) * 0.875)
	if got.MemoryHighBytes != wantHigh {
		t.Errorf("MemoryHighBytes = %d, want %d", got.MemoryHighBytes, wantHigh)
	}
	// cpu.max = capacity.cpu * 100000us
	if got.CPUMaxQuotaUs != 2*100000 {
		t.Errorf("CPUMaxQuotaUs = %d, want %d", got.CPUMaxQuotaUs, 2*100000)
	}
	// cpu.weight = round(1.5 * 100) = 150
	if got.CPUWeight != 150 {
		t.Errorf("CPUWeight = %d, want 150", got.CPUWeight)
	}
}
