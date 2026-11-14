package main

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Keep raw slot devices out of application processes: buffered writes made
// through an unbound device node could otherwise arrive after publication.
// The cgroup-device hook matches device numbers, including alternate nodes.
// sandbox-init retains access in the Guest-global cgroup.
type blockDevice struct{ major, minor uint32 }
type deviceInsn struct {
	op, registers uint8
	offset        int16
	immediate     int32
}

func deviceFilter(devices []blockDevice) []deviceInsn {
	const (
		// bpf_cgroup_dev_ctx contains access_type, major, minor (three u32).
		load = unix.BPF_LDX | unix.BPF_MEM | unix.BPF_W
		move = unix.BPF_ALU64 | unix.BPF_K | unix.BPF_MOV
		jne  = unix.BPF_JMP | unix.BPF_K | unix.BPF_JNE
		exit = unix.BPF_JMP | unix.BPF_EXIT
	)
	prog := []deviceInsn{
		{move, 0, 0, 1},    // R0 = allow
		{load, 0x12, 0, 0}, // R2 = *(u32 *)(R1 + 0)
		{unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, 2, 0, 0xffff},
		{jne, 2, int16(2 + 4*len(devices)), unix.BPF_DEVCG_DEV_BLOCK},
		{load, 0x12, 4, 0}, // R2 = major
		{load, 0x13, 8, 0}, // R3 = minor
	}
	for _, dev := range devices {
		prog = append(prog,
			deviceInsn{jne, 2, 3, int32(dev.major)},
			deviceInsn{jne, 3, 2, int32(dev.minor)},
			deviceInsn{move, 0, 0, 0}, // deny this slot
			deviceInsn{op: exit})
	}
	return append(prog, deviceInsn{op: exit})
}

func (r *diskSlots) protectDevices() error {
	r.RLock()
	defer r.RUnlock()
	if len(r.disks) == 0 {
		return nil
	}
	var devices []blockDevice
	for _, d := range r.disks {
		var st unix.Stat_t
		if err := unix.Stat(d.spec.DiskDevs[0], &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFBLK {
			return fmt.Errorf("slot device %s is not a block device", d.spec.DiskDevs[0])
		}
		devices = append(devices, blockDevice{unix.Major(st.Rdev), unix.Minor(st.Rdev)})
	}
	return attachDeviceFilter(devices)
}

func attachDeviceFilter(devices []blockDevice) error {
	prog := deviceFilter(devices)
	license := []byte("GPL\x00")
	// Prefix of union bpf_attr for BPF_PROG_LOAD.
	attr := struct {
		Type, Count               uint32
		Instructions, License     uint64
		LogLevel, LogSize         uint32
		LogBuf                    uint64
		KernVersion, Flags        uint32
		Name                      [16]byte
		Interface, ExpectedAttach uint32
	}{Type: unix.BPF_PROG_TYPE_CGROUP_DEVICE, Count: uint32(len(prog)),
		Instructions: uint64(uintptr(unsafe.Pointer(&prog[0]))), License: uint64(uintptr(unsafe.Pointer(&license[0]))),
		ExpectedAttach: unix.BPF_CGROUP_DEVICE}
	fd, _, errno := unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_LOAD, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr))
	runtime.KeepAlive(prog)
	runtime.KeepAlive(license)
	if errno != 0 {
		return fmt.Errorf("load slot device filter: %w", errno)
	}
	defer unix.Close(int(fd))
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	if applicationCgroups.root == nil {
		return errors.New("application cgroup unavailable")
	}
	args := struct{ Target, Prog, Attach, Flags uint32 }{uint32(applicationCgroups.root.Fd()), uint32(fd), unix.BPF_CGROUP_DEVICE, 0}
	_, _, errno = unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_ATTACH, uintptr(unsafe.Pointer(&args)), unsafe.Sizeof(args))
	if errno != 0 {
		return fmt.Errorf("install slot device filter: %w", errno)
	}
	return nil
}
