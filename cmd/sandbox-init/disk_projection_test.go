package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func TestDiskProjectionValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/run", filepath.Join(root, "var-run")); err != nil {
		t.Fatal(err)
	}
	disk := func(i int, sub, target string) proto.MountSpec {
		return proto.MountSpec{Type: "disk", Subpath: sub, Target: target, DiskIndex: i, DiskDevs: []string{"/dev/vdb"}}
	}
	for _, c := range []struct {
		name   string
		mounts []proto.MountSpec
		ok     bool
	}{
		{"disjoint", []proto.MountSpec{disk(0, "data", "/work"), disk(0, "database", "/worker")}, true},
		{"same-source", []proto.MountSpec{disk(0, "a/./b", "/data"), disk(0, "a/b/", "/results")}, false},
		{"nested-source", []proto.MountSpec{disk(0, "a", "/data"), disk(0, "a/b", "/results")}, false},
		{"different-disks", []proto.MountSpec{disk(0, "a", "/data"), disk(1, "a", "/results")}, true},
		{"disk-root", []proto.MountSpec{disk(0, "", "/data"), disk(0, "a", "/results")}, false},
		{"other-mount", []proto.MountSpec{disk(0, "", "/run/project"), {Type: "tmpfs", Target: "/var-run/project/cache"}}, false},
		{"tmpfs-alias", []proto.MountSpec{disk(0, "", "/var-run")}, false},
		{"tmpfs-child", []proto.MountSpec{disk(0, "", "/run/project")}, true},
		{"sysfs", []proto.MountSpec{disk(0, "", "/sys")}, false},
		{"dotdot", []proto.MountSpec{disk(0, "ok/../bad", "/data")}, false},
		{"absolute", []proto.MountSpec{disk(0, "/a", "/data")}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := validateDiskMounts(root, filepath.Join(root, "run"), c.mounts)
			if (err == nil) != c.ok {
				t.Fatalf("got %v, want ok=%t", err, c.ok)
			}
		})
	}
}

func TestOpenDiskDirectoryPinsSource(t *testing.T) {
	root := t.TempDir()
	fd, err := openDiskDirectory(root, "data/more")
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	orig, renamed := filepath.Join(root, "data"), filepath.Join(root, "old")
	if err = os.Rename(orig, renamed); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), orig); err != nil {
		t.Fatal(err)
	}
	file, err := unix.Openat(fd, "written", unix.O_CREAT|unix.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(file)
	if _, err = os.Stat(filepath.Join(renamed, "more/written")); err != nil {
		t.Fatal(err)
	}
	if _, err = openDiskDirectory(root, "data/more"); err == nil {
		t.Fatal("followed replacement symlink")
	}
	if _, err = openDiskDirectory(root, "old/more/written"); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("non-directory: %v", err)
	}
	if _, err = openDiskDirectory(root, "../a"); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("parent accepted: %v", err)
	}
}
func TestDiskBindReplay(t *testing.T) {
	b := proto.DiskBinding{DiskIndex: 0, BindingID: "original", SizeBytes: 4096}
	r := diskSlots{disks: map[int]*diskSlot{0: {binding: b}}}
	if err := r.confirm(b); err != nil {
		t.Fatal(err)
	}
	b.SizeBytes = 8192
	if err := r.confirm(b); err == nil {
		t.Fatal("changed size accepted")
	}
	b.SizeBytes = 4096
	b.BindingID = "replacement"
	if err := r.confirm(b); err == nil {
		t.Fatal("replacement accepted")
	}
	var s diskSlot
	if err := s.project("", -1); !errors.Is(err, unix.EAGAIN) {
		t.Fatal(err)
	}
}
