package main

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// managedFilesystem pins the actual writable ext4 before application access.
// The open file description also owns syncfs's writeback-error cursor, so it
// must survive quiesce retries and memory restore; reopening a path is unsafe.
type managedFilesystem struct {
	disk string
	file *os.File
	dev  uint64
}

type managedFilesystems struct {
	mu    sync.Mutex
	files []*managedFilesystem
	// A writeback error may be reported only once. Retain it for this Guest
	// generation: a later zero result does not prove the lost write recovered.
	failure error
}

var guestFilesystems managedFilesystems

func (r *managedFilesystems) register(disk, path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		_ = f.Close()
		return err
	}
	if uint64(fs.Type) != unix.EXT4_SUPER_MAGIC {
		_ = f.Close()
		return fmt.Errorf("managed disk %s is not its writable ext4 filesystem", disk)
	}
	return r.add(disk, f, uint64(st.Dev))
}

// add consumes file on every path. Managed disks do not change filesystem
// identity during a Guest lifetime; cold launch builds a new registry.
func (r *managedFilesystems) add(disk string, file *os.File, dev uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, old := range r.files {
		if old.disk == disk && old.dev != dev {
			_ = file.Close()
			return fmt.Errorf("managed disk %s changed filesystem identity", disk)
		}
		if old.dev == dev {
			return file.Close()
		}
	}
	r.files = append(r.files, &managedFilesystem{disk: disk, file: file, dev: dev})
	return nil
}

func (r *managedFilesystems) sync() error { return r.syncWith(unix.Syncfs) }

// Management checks a poisoned generation before contacting the inner or
// changing execution/observation admission. The actual sync repeats this
// check under the same cursor lock.
func (r *managedFilesystems) checkCaptureStart() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *managedFilesystems) syncWith(syncfs func(int) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if len(r.files) == 0 {
		return errors.New("quiesce: no managed writable root filesystem registered")
	}
	// Keep the fixed registry and each open-file-description's error cursor
	// serialized until every synchronous syscall has actually returned.
	for _, fs := range r.files {
		if err := syncfs(int(fs.file.Fd())); err != nil {
			err = fmt.Errorf("quiesce: syncfs %s: %w", fs.disk, err)
			if r.failure == nil {
				r.failure = err
			}
			return r.failure
		}
	}
	return nil
}
