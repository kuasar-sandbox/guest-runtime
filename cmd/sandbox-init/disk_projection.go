package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/cyphar/filepath-securejoin"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func nested(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

// Only a projection of a declared slot can conflict with another mount. DiskSlot
// also identifies unbound slots sent by hosts predating DiskSlotDeclared.
func validateDiskMounts(root, runDir string, spec *proto.LaunchSpec) error {
	mounts := spec.Mounts
	slot := func(m proto.MountSpec) bool { return m.Type == "disk" && (m.DiskSlotDeclared || m.DiskSlot) }
	checkTargets := slices.ContainsFunc(mounts, slot)
	var fixedTargets []string
	if checkTargets {
		for _, path := range []string{"dev", "sys", "proc", "opt/sandbox-runtime"} {
			resolved, err := securejoin.SecureJoin(root, path)
			if err != nil {
				return err
			}
			fixedTargets = append(fixedTargets, resolved)
		}
	}
	type target struct {
		path string
		slot bool
	}
	targets := make([]target, 0, len(mounts))
	disks := make(map[int]proto.MountSpec)
	sources := make(map[int][]string)
	for _, m := range mounts {
		if checkTargets {
			resolved, err := securejoin.SecureJoin(root, m.Target)
			if err != nil {
				return err
			}
			targets = append(targets, target{resolved, slot(m)})
			if slot(m) {
				// System mounts installed during switch-root would hide a projection.
				if nested(resolved, runDir) || nested(resolved, filepath.Join(runDir, "shm")) {
					return fmt.Errorf("disk %d target %q conflicts with guest mount at %s", m.DiskIndex, m.Target, resolved)
				}
				for _, reserved := range fixedTargets {
					if nested(reserved, resolved) || nested(resolved, reserved) {
						return fmt.Errorf("disk %d target %s overlaps guest mount %s", m.DiskIndex, m.Target, reserved)
					}
				}
			}
		}
		if m.Type != "disk" {
			continue
		}
		if !filepath.IsAbs(m.Target) {
			return fmt.Errorf("disk %d target must be absolute: %q", m.DiskIndex, m.Target)
		}
		wantDevs := 1
		if m.DiskOverlay {
			wantDevs = 2
		}
		if m.DiskIndex < 0 || m.DiskIndex >= 8 || len(m.DiskDevs) != wantDevs || (m.DiskSlot && m.DiskOverlay) {
			return fmt.Errorf("invalid disk %d device topology", m.DiskIndex)
		}
		if old, ok := disks[m.DiskIndex]; ok && (slot(old) != slot(m) || old.DiskSlot != m.DiskSlot || old.DiskOverlay != m.DiskOverlay || !slices.Equal(old.DiskDevs, m.DiskDevs)) {
			return fmt.Errorf("inconsistent device topology for disk %d", m.DiskIndex)
		}
		disks[m.DiskIndex] = m
		if filepath.IsAbs(m.Subpath) || slices.Contains(strings.Split(m.Subpath, "/"), "..") {
			return fmt.Errorf("disk %d invalid subpath %q", m.DiskIndex, m.Subpath)
		}
		if slot(m) {
			sources[m.DiskIndex] = append(sources[m.DiskIndex], filepath.Clean("/"+m.Subpath))
		}
	}
	// The trailing slash makes each subtree contiguous even for names such
	// as "a", "a-b", "a/n". Keep active ancestors to allow tmpfs-on-tmpfs.
	for i := range targets {
		targets[i].path = strings.TrimRight(targets[i].path, "/") + "/"
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	var ancestors []target
	for _, t := range targets {
		for len(ancestors) > 0 && !strings.HasPrefix(t.path, ancestors[len(ancestors)-1].path) {
			ancestors = ancestors[:len(ancestors)-1]
		}
		if len(ancestors) > 0 && (t.slot || ancestors[len(ancestors)-1].slot) {
			return fmt.Errorf("mount %s overlaps %s", t.path, ancestors[len(ancestors)-1].path)
		}
		ancestors = append(ancestors, t)
	}
	for i, subs := range sources {
		for j := range subs {
			subs[j] = strings.TrimRight(subs[j], "/") + "/"
		}
		slices.Sort(subs)
		for j := 1; j < len(subs); j++ {
			if strings.HasPrefix(subs[j], subs[j-1]) {
				return fmt.Errorf("disk %d subpath %s overlaps %s", i, subs[j], subs[j-1])
			}
		}
	}
	return nil
}

func fdPath(fd int) string { return fmt.Sprintf("/proc/self/fd/%d", fd) }

// openDiskDirectory pins every directory before operating on its children.
// A rename cannot redirect the bind, and an existing symlink or submount fails.
func openDiskDirectory(root, subpath string) (int, error) {
	if filepath.IsAbs(subpath) || slices.Contains(strings.Split(subpath, "/"), "..") {
		return -1, unix.EINVAL
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	how := &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV}
	for _, part := range strings.Split(filepath.Clean(subpath), "/") {
		if part == "." {
			continue
		}
		next, err := unix.Openat2(fd, part, how)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, part, 0755); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat2(fd, part, how)
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func projectDiskDirectory(root, subpath, target string) error {
	fd, err := openDiskDirectory(root, subpath)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Mount(fdPath(fd), target, "", unix.MS_BIND, "")
}
