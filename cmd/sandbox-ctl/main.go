// sandbox-ctl is the host-side control tool for one sandbox lifecycle.
// Subcommands:
//   run       — cold-start a sandbox from sandbox.yaml
//   snapshot  — pause + dump to sandbox.snapshot + disk.ext4
//   restore   — start from a sandbox.snapshot via uffd lazy memory load
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
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
	case "restore":
		os.Exit(restoreCmd(os.Args[2:]))
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
  sandbox-ctl run       --config sandbox.yaml [--accelerator-config accelerator.yaml] [--sandbox-id <sid>] [--ch-binary <path>]
  sandbox-ctl snapshot  --sandbox-id <sid> --output <out_dir> [--upload]
  sandbox-ctl restore   --snapshot <file_path> --config <path> [--sandbox-id <sid>]

Run cold-starts one sandbox VM and blocks until the guest exits. See
docs/sandbox-design.md for the full design.
`)
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to sandbox.yaml (required)")
	accelPath := fs.String("accelerator-config", "", "path to accelerator.yaml (default: search ./accelerator.yaml then ~/.config/...)")
	sandboxID := fs.String("sandbox-id", "", "sandbox id (overrides sandbox.yaml)")
	chBinary := fs.String("ch-binary", "cloud-hypervisor", "path to cloud-hypervisor binary")
	runtimeRoot := fs.String("runtime-root", "/run", "directory under which /<sid>/{ch,blk0,blk1}.sock are created")
	statsJSON := fs.String("stats-json", "", "if set, write vhost-blk per-backend stats as JSON to this path on shutdown")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl run: --config is required")
		return 2
	}

	cfg, err := sandbox.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	accelCfg, err := sandbox.LoadAccelerator(*accelPath)
	if err != nil {
		// Not fatal: only manifest:// disks need it; we proceed with nil.
		fmt.Fprintf(os.Stderr, "[sandbox-ctl] warn: accelerator config not loaded: %v (manifest:// disks will fail)\n", err)
		accelCfg = nil
	}

	ctx, cancel := signalContext()
	defer cancel()

	exit, err := sandbox.Run(ctx, sandbox.RunOptions{
		Cfg:           cfg,
		AccelCfg:      accelCfg,
		SandboxID:     *sandboxID,
		CHBinary:      *chBinary,
		RuntimeRoot:   *runtimeRoot,
		StatsJSONPath: *statsJSON,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
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
