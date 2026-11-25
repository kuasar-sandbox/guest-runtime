package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSystemdBootBarrier(t *testing.T) {
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, "sysinit.target.d", "50-sandbox-init.conf")

	if err := prepareSystemdBootBarrier(unitDir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != systemdSysinitDropIn {
		t.Fatalf("drop-in content = %q, want %q", got, systemdSysinitDropIn)
	}
	if err := prepareSystemdBootBarrier(unitDir); err != nil {
		t.Fatalf("identical repeated preparation: %v", err)
	}
}

func TestPrepareSystemdBootBarrierRejectsConflict(t *testing.T) {
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, "sysinit.target.d", "50-sandbox-init.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const conflict = "[Unit]\nAfter=network.target\n"
	if err := os.WriteFile(path, []byte(conflict), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSystemdBootBarrier(unitDir); err == nil {
		t.Fatal("conflicting drop-in was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != conflict {
		t.Fatalf("conflicting file was modified: %q", got)
	}
}

func TestPrepareSystemdBootBarrierRejectsLowerPriorityConflict(t *testing.T) {
	unitDir := t.TempDir()
	imageUnitDir := t.TempDir()
	path := filepath.Join(imageUnitDir, "sysinit.target.d", "50-sandbox-init.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const conflict = "[Unit]\nBefore=network.target\n"
	if err := os.WriteFile(path, []byte(conflict), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSystemdBootBarrier(unitDir, imageUnitDir); err == nil {
		t.Fatal("lower-priority conflicting drop-in was silently shadowed")
	}
	if _, err := os.Stat(filepath.Join(unitDir, "sysinit.target.d", "50-sandbox-init.conf")); !os.IsNotExist(err) {
		t.Fatalf("runtime drop-in should not be written after conflict: %v", err)
	}
}

func TestPrepareSystemdBootBarrierRejectsSymlink(t *testing.T) {
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, "sysinit.target.d", "50-sandbox-init.conf")
	target := filepath.Join(unitDir, "target")
	if err := os.WriteFile(target, []byte(systemdSysinitDropIn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := prepareSystemdBootBarrier(unitDir); err == nil {
		t.Fatal("symlink drop-in was accepted")
	}
}
