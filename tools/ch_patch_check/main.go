// ch_patch_check — verification probes for the cloud-hypervisor patch
// series in deps/ch-patches/ (memory-zone fd= injection,
// snapshot skip user-managed, external uffd handler).
//
// Each probe is a sub-mode selected by argv[1]; runnable independently.
// All probes assume bin/cloud-hypervisor is already built with patches
// applied (make cloud-hypervisor) and bin/vmlinux exists.
//
// Probes:
//
//   parse-only
//     Spawn CH with --memory-zone size=4M,fd=999,uffd_fd=998,uffd_socket=...
//     The fd is bogus, so boot fails at MemoryManager — but the parser
//     must accept the new keys (no "unknown key" error before the fd
//     attempt).
//
//   memfd-boot
//     Create a real memfd, ftruncate, madvise(NOHUGE), spawn CH with
//     --memory-zone fd=N (no uffd). Verify the kernel boots far enough
//     to hit a known printk line (e.g. "Run /sbin/init").
//
//   snapshot-skip
//     Boot via memfd-boot, then issue /vm.pause + /vm.snapshot. Verify
//     the destination directory contains config.json + state.json but
//     NOT memory-ranges.
//
//   uffd-dummy
//     Set up a userfaultfd, listen on a UDS for va_report, ack on
//     receive. Spawn CH with --memory-zone fd=N,uffd_fd=M,uffd_socket=...
//     Verify (a) we receive va_report, (b) UFFDIO_COPY zero satisfies
//     CH's first faults, (c) kernel boots far enough.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	args := os.Args[2:]
	ctx := context.Background()

	var err error
	switch mode {
	case "parse-only":
		err = runParseOnly(ctx, args)
	case "memfd-boot":
		err = runMemfdBoot(ctx, args)
	case "snapshot-skip":
		err = runSnapshotSkip(ctx, args)
	case "uffd-dummy":
		err = runUffdDummy(ctx, args)
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %q\n\n", mode)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", mode, err)
		os.Exit(1)
	}
	fmt.Printf("PASS %s\n", mode)
}

func usage() {
	fmt.Fprintf(os.Stderr, `ch_patch_check — verify cloud-hypervisor patches

Usage:
  ch_patch_check <mode> [flags]

Modes:
  parse-only       parser accepts fd=/uffd_fd=/uffd_socket=
  memfd-boot       boot kernel from sandbox-ctl-allocated memfd
  snapshot-skip    /vm.snapshot omits memory-ranges for fd-backed zone
  uffd-dummy       full path: memfd + uffd handler dummy + boot

Common flags (each mode has its own flag.FlagSet):
  --ch-binary    path to cloud-hypervisor (default: bin/cloud-hypervisor)
  --vmlinux      path to vmlinux         (default: bin/vmlinux)
  --keep         don't clean tmp dir
  --timeout      wallclock timeout (default mode-specific)
`)
}

// commonFlags wraps flags shared across probes.
type commonFlags struct {
	chBin   string
	vmlinux string
	keep    bool
	timeout int
}

func (cf *commonFlags) bind(fs *flag.FlagSet, defaultTimeout int) {
	fs.StringVar(&cf.chBin, "ch-binary", "bin/cloud-hypervisor", "path to cloud-hypervisor")
	fs.StringVar(&cf.vmlinux, "vmlinux", "bin/vmlinux", "path to kernel image")
	fs.BoolVar(&cf.keep, "keep", false, "don't remove temp dirs on success")
	fs.IntVar(&cf.timeout, "timeout", defaultTimeout, "timeout seconds")
}
