package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func TestVolumeBindsUnderRun(t *testing.T) {
	const child = "SANDBOX_INIT_TEST_RUN_MOUNTS"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestVolumeBindsUnderRun$")
		cmd.Env = append(os.Environ(), child+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
		if out, err := cmd.CombinedOutput(); errors.Is(err, unix.EPERM) {
			t.Skip("private mount namespace unavailable")
		} else if err != nil {
			t.Fatalf("private mount namespace: %v\n%s", err, out)
		}
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	root, source := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "marker"), []byte("volume"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := mountRunDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(run, unix.MNT_DETACH)
	runMount, err := os.Stat(run)
	if err != nil {
		t.Fatal(err)
	}
	shmMount, err := os.Stat(filepath.Join(run, "shm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/run", "/run/shm", "/var/run/service", "/run/shm/service"} {
		if err := bindVolumeDir(root, run, source, target); err != nil {
			t.Fatalf("bind %s: %v", target, err)
		}
	}
	if err := ensureRunDirs(root, runMount, shmMount); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{run, filepath.Join(run, "shm")} {
		if _, err := os.Stat(filepath.Join(dir, "marker")); !os.IsNotExist(err) {
			t.Fatalf("automatic tmpfs at %s masked: %v", dir, err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "service/marker")); err != nil || string(data) != "volume" {
			t.Fatalf("volume under %s: %q, %v", dir, data, err)
		}
	}

	// A data volume can cover the physical parent of an image's /run alias.
	// Use a separate root so both symlink directions are exercised.
	for _, parent := range []string{"/var", "/"} {
		t.Run("cover"+parent, func(t *testing.T) {
			other := t.TempDir()
			if err := os.Mkdir(filepath.Join(other, "var"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("var/run", filepath.Join(other, "run")); err != nil {
				t.Fatal(err)
			}
			autoRun, err := mountRunDirs(other)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Unmount(autoRun, unix.MNT_DETACH)
			workloadRun, err := os.Stat(autoRun)
			if err != nil {
				t.Fatal(err)
			}
			workloadShm, err := os.Stat(filepath.Join(autoRun, "shm"))
			if err != nil {
				t.Fatal(err)
			}
			if err := bindVolumeDir(other, autoRun, source, parent); err != nil {
				t.Fatal(err)
			}
			defer unix.Unmount(filepath.Join(other, parent), unix.MNT_DETACH)
			if err := ensureRunDirs(other, workloadRun, workloadShm); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"run", "run/shm"} {
				var st unix.Statfs_t
				if err := unix.Statfs(filepath.Join(other, path), &st); err != nil || st.Type != unix.TMPFS_MAGIC {
					t.Fatalf("automatic %s after binding %s: filesystem %x, %v", path, parent, st.Type, err)
				}
			}
		})
	}
}

func TestVolumeDirFollowsWorkloadRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	target, err := prepareVolumeDir(root, "/var/run/service")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "run/service")
	if target != want {
		t.Fatalf("bind target = %q, want %q", target, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("workload directory = %v, %v", fi, err)
	}
}

func TestFileInjectionPreparesDanglingReferent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "etc/resolv.conf")
	const link = "../run/resolver/resolv.conf"
	if err := os.Symlink(link, path); err != nil {
		t.Fatal(err)
	}
	if err := ensureTargetFile(path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(path); err != nil || got != link {
		t.Fatalf("symlink = %q, %v", got, err)
	}
	target := filepath.Join(root, "run/resolver/resolv.conf")
	if data, err := os.ReadFile(target); err != nil || len(data) != 0 {
		t.Fatalf("prepared target = %q, %v", data, err)
	}
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureTargetFile(path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("existing content = %q, %v", data, err)
	}
}

func TestInitUsesConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	const name = "init-path-test"
	executable := filepath.Join(dir, name)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s' \"$VALUE\" > result\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	spec := proto.InitSpec{Exec: name, Workdir: dir, Env: map[string]string{"PATH": dir, "VALUE": "child"}}
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "result")); err != nil || string(data) != "child" {
		t.Fatalf("result = %q, %v", data, err)
	}
	spec.Exec = "true"
	spec.Env = map[string]string{"VALUE": "keep-default-path"}
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatalf("default PATH: %v", err)
	}
	t.Setenv("PATH", dir)
	spec.Exec = name
	spec.Env["PATH"] = ""
	if err := runInit([]proto.InitSpec{spec}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("explicit empty PATH: %v", err)
	}
	spec.Exec = "./" + name
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatalf("explicit relative executable: %v", err)
	}
	spec.Exec = name
	spec.Env["PATH"] = "."
	if err := runInit([]proto.InitSpec{spec}); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("implicit relative executable: %v", err)
	}
}
