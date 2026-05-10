// sandbox-ctl is the host-side control tool for one sandbox lifecycle.
// Subcommands:
//
//	run       — start a sandbox (cold-start; or with --restore=<ref> from a snapshot)
//	snapshot  — pause + dump to <sid>.snapshot + <sha256>.overlay (or upload)
//
// See docs/sandbox.md for the full design.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func init() {
	// Microsecond precision so timing analysis (cold-start latency,
	// vhost handshake delta, vsock launch handshake) is computable
	// directly from log timestamps.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "snapshot":
		os.Exit(snapshotCmd(os.Args[2:]))
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `sandbox-ctl — sandbox runtime control

Usage:
  sandbox-ctl run       --config sandbox.yaml [--manifest-config <path>]
                        [--sandbox-id <sid>] [--ch-binary <path>] [--run-dir <dir>]
                        [--restore <file_path|manifest://hex>]
                        [--stdin] [--stdout=false] [--stderr=false]
                        [--stdin-from F] [--stdout-to F] [--stderr-to F]
                        [--tty]
  sandbox-ctl snapshot  --sandbox-id <sid> (--output <out_dir> | --upload)
                        [--resume] [--run-dir <dir>] [--timeout <sec>]

--manifest-config (or MANIFEST_CONFIG env) is required for any
manifest:// resource (boot.root.base, --restore manifest://, --upload).
file://-only configurations may omit it.

run starts one sandbox VM and blocks until the guest exits. With
--restore, the sandbox is resumed from a snapshot bundle instead of
cold-starting (sandbox.yaml field semantics in restore mode are listed
in docs/sandbox.md §11.0).

snapshot pauses a running sandbox and writes a snapshot bundle either
to a local directory (--output) or to the manifest store (--upload).
The two are mutually exclusive. By default the sandbox is destroyed
after a successful snapshot; use --resume to keep it running.

See docs/sandbox.md for the full design.
`)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()
	return ctx, cancel
}
