package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// Linux autofs_dev_ioctl (v1), shared by amd64 and arm64. Only READY and
// FAIL are needed: mounts live for the Guest lifetime and never expire.
type autofsReply struct {
	Major, Minor, Size uint32
	MountFD            int32
	Token              uint32
	Status             int32
}

const (
	autofsReady         = 0xc0189376
	autofsFail          = 0xc0189377
	autofsPacketSize    = 304 // struct autofs_v5_packet on the two guest architectures
	autofsMissingDirect = 5
)

func diskSlotSupported() bool {
	var st unix.Stat_t
	return unix.Stat("/dev/autofs", &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFCHR
}

type diskSlots struct {
	sync.RWMutex
	disks map[int]*diskSlot
}

var guestSlots = diskSlots{disks: make(map[int]*diskSlot)}

type diskSlot struct {
	mu        sync.Mutex // serializes each trigger's decision AND kernel reply with bind ACKs
	spec      proto.MountSpec
	options   string
	stagingFD int // parent is pinned; re-open the leaf after mounting over it
	binding   proto.DiskBinding
	mounted   bool
	fault     error
}

func (r *diskSlots) install(spec proto.MountSpec, dir, options string) error {
	r.Lock()
	defer r.Unlock()
	// Linux starts the initial PID 1 in the bootstrap process group (PGID 0).
	// autofs requires a real, live daemon group. Descendant workloads get a
	// distinct group when forked and must never inherit this daemon identity.
	if unix.Getpgrp() == 0 {
		if err := unix.Setpgid(0, 0); err != nil {
			return fmt.Errorf("establish autofs daemon process group: %w", err)
		}
	}
	slot := r.disks[spec.DiskIndex]
	if slot == nil {
		if err := waitForDevice(spec.DiskDevs[0], devicePollTimeout); err != nil {
			return err
		}
		parentFD, err := unix.Open("/sysdisks", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		slot = &diskSlot{spec: spec, options: options, stagingFD: parentFD}
		r.disks[spec.DiskIndex] = slot
	}
	return slot.installTrigger(dir, spec.Subpath)
}

func (s *diskSlot) installTrigger(dir, subpath string) error {
	ctl, err := unix.Open("/dev/autofs", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		unix.Close(ctl)
		return err
	}
	options := fmt.Sprintf("fd=%d,pgrp=%d,minproto=5,maxproto=5,direct", writer.Fd(), unix.Getpgrp())
	err = unix.Mount("sandbox-disk", dir, "autofs", unix.MS_NOSUID|unix.MS_NODEV, options)
	writer.Close()
	if err != nil {
		reader.Close()
		unix.Close(ctl)
		return err
	}
	// The daemon's process group can open its own direct trigger without
	// firing it. This fd pins the precise mount across rename and switch-root.
	mountFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		reader.Close()
		unix.Close(ctl)
		return err
	}
	go s.serveTrigger(reader, ctl, mountFD, subpath)
	return nil
}

func (r *diskSlots) confirm(binding proto.DiskBinding) error {
	if binding.BindingID == "" || binding.SizeBytes == 0 {
		return errors.New("invalid disk binding identity or size")
	}
	r.RLock()
	slot := r.disks[binding.DiskIndex]
	r.RUnlock()
	if slot == nil {
		return fmt.Errorf("unknown disk slot %d", binding.DiskIndex)
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.fault != nil {
		return slot.fault
	}
	if slot.binding.BindingID != "" {
		if slot.binding != binding {
			return errors.New("disk slot already has a different binding")
		}
		return nil
	}
	fd, err := unix.Open(slot.spec.DiskDevs[0], unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var size uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return errno
	}
	if size != binding.SizeBytes {
		return fmt.Errorf("disk %d size %d, expected %d", binding.DiskIndex, size, binding.SizeBytes)
	}
	slot.binding = binding
	return nil
}

func (s *diskSlot) project(subpath string, mountFD int) error {
	if s.binding.BindingID == "" {
		return unix.EAGAIN
	}
	staging := filepath.Join(fdPath(s.stagingFD), fmt.Sprintf("disk-%d", s.spec.DiskIndex))
	if !s.mounted {
		if err := assembleDataDisk(s.spec, staging, s.options); err != nil {
			return err
		}
		s.mounted = true
	}
	return projectDiskDirectory(staging, subpath, fdPath(mountFD))
}

func (s *diskSlot) serveTrigger(reader *os.File, ctl, mountFD int, subpath string) {
	defer reader.Close()
	defer unix.Close(ctl)
	defer unix.Close(mountFD)
	var packet [autofsPacketSize]byte
	for {
		// The kernel emits one atomic (<= PIPE_BUF) packet per request.
		n, err := reader.Read(packet[:])
		if err != nil || n != len(packet) || binary.NativeEndian.Uint32(packet[:4]) != 5 || binary.NativeEndian.Uint32(packet[4:8]) != autofsMissingDirect {
			s.mu.Lock()
			s.fault = fmt.Errorf("disk %d lost autofs trigger (size %d, read %v)", s.spec.DiskIndex, n, err)
			s.mu.Unlock()
			break
		}
		s.mu.Lock()
		err = s.project(subpath, mountFD)
		reply := autofsReply{Major: 1, Minor: 1, Size: uint32(unsafe.Sizeof(autofsReply{})), MountFD: int32(mountFD), Token: binary.NativeEndian.Uint32(packet[8:12])}
		op := uintptr(autofsReady)
		if err != nil {
			op = autofsFail
			errno := unix.EIO
			var cause unix.Errno
			if errors.As(err, &cause) {
				errno = cause
			}
			reply.Status = -int32(errno)
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(ctl), op, uintptr(unsafe.Pointer(&reply)))
		if errno != 0 {
			s.fault = fmt.Errorf("autofs reply disk %d: %w", s.spec.DiskIndex, errno)
		}
		s.mu.Unlock()
		if errno != 0 {
			break
		}
	}
	// CATATONIC wakes kernel waiters even if reading or replying fails.
	_ = unix.IoctlSetInt(mountFD, 0x9362, 0)
}
