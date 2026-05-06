package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// runMemfdBoot probes commit memory-zone fd injection: CH uses our externally-allocated
// memfd as the memory zone backing.
//
// Method:
//  1. memfd_create(MFD_CLOEXEC)
//  2. ftruncate to RAM size
//  3. madvise MADV_NOHUGEPAGE on the host-side mmap (would matter for
//     uffd; here we keep it for parity with the production path)
//  4. spawn CH with --memory-zone size=...,fd=3,shared=on  (fd=3 via ExtraFiles)
//  5. enable RUST_LOG=info; watch for the "RAM region mapping at 0x" line
//     emitted from create_ram_region_raw — proves CH took our memfd
//  6. SIGTERM, reap
//
// We don't actually boot to userland. The kernel may or may not progress
// (no rootfs, no pmem), but the marker fires before any of that matters.
func runMemfdBoot(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("memfd-boot", flag.ContinueOnError)
	var cf commonFlags
	cf.bind(fs, 30)
	memMB := fs.Int("mem-mb", 256, "RAM size in MiB")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	size := int64(*memMB) << 20

	// Step 1-2: memfd + ftruncate.
	memfd, err := unix.MemfdCreate("ch-patch-check", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(memfd)
	if err := unix.Ftruncate(memfd, size); err != nil {
		return fmt.Errorf("ftruncate: %w", err)
	}

	// Step 3: parent-side mmap + MADV_NOHUGEPAGE. CH will mmap the same
	// inode; THP setting on this VMA doesn't affect CH's VMA but we
	// exercise the path the production sandbox-ctl will take.
	addr, err := unix.Mmap(memfd, 0, int(size),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap memfd: %w", err)
	}
	defer unix.Munmap(addr)
	if err := unix.Madvise(addr, unix.MADV_NOHUGEPAGE); err != nil {
		return fmt.Errorf("madvise NOHUGEPAGE: %w", err)
	}

	// Step 4: spawn CH with ExtraFiles[0]=memfd → fd=3 in child.
	memfdFile := os.NewFile(uintptr(memfd), "memfd")
	defer memfdFile.Close()

	mon := newMonitorReader("ch", "RAM region mapping at 0x")

	args := []string{
		"--kernel", cf.vmlinux,
		"--memory", "size=0",
		"--memory-zone",
		fmt.Sprintf("id=z0,size=%dM,shared=on,fd=3", *memMB),
		"--cpus", "boot=1",
		"--serial", "tty",
		"--console", "off",
		"-vv",
	}

	subCtx, cancel := context.WithTimeout(ctx, time.Duration(cf.timeout)*time.Second)
	defer cancel()
	cmd, err := startCH(subCtx, cf, mon, args, []*os.File{memfdFile})
	if err != nil {
		return fmt.Errorf("spawn CH: %w", err)
	}

	// Step 5: wait for the marker, or timeout.
	select {
	case <-mon.hit:
		fmt.Fprintln(stderrSink, "==> marker hit: RAM region mapped from our memfd")
	case <-subCtx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("timeout waiting for RAM-region marker (%ds)", cf.timeout)
	}

	// Step 6: reap.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}

	return nil
}
