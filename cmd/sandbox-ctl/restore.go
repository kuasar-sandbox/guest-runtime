package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/restore"
)

func restoreCmd(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	snapshotURI := fs.String("snapshot", "", "sandbox.snapshot reference: file path or manifest://<hex> (required)")
	configPath := fs.String("config", "", "host sandbox.yaml (provides TAP, overlay.diff)")
	accelPath := fs.String("accelerator-config", "", "path to accelerator.yaml (required when --snapshot or sandbox.cfg disks use manifest://)")
	sandboxID := fs.String("sandbox-id", "", "override sandbox id")
	chBinary := fs.String("ch-binary", "cloud-hypervisor", "path to cloud-hypervisor")
	runtimeRoot := fs.String("runtime-root", "/run", "directory for /<sid>/")
	statsJSON := fs.String("stats-json", "", "if set, write per-backend + uffd stats as JSON to this path on shutdown")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *snapshotURI == "" {
		fmt.Fprintln(os.Stderr, "restore: --snapshot required")
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "restore: --config required (host yaml)")
		return 2
	}

	cfg, err := sandbox.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Decompose --snapshot into either a file path or a manifest hex key.
	const manifestPrefix = "manifest://"
	var (
		snapshotPath string
		snapshotKey  string
	)
	if strings.HasPrefix(*snapshotURI, manifestPrefix) {
		snapshotKey = strings.TrimPrefix(*snapshotURI, manifestPrefix)
	} else {
		snapshotPath = *snapshotURI
	}

	// Open accelerator runtime when manifest:// is in play. We can't yet
	// inspect sandbox.cfg's disk references (that's inside the snapshot
	// bundle), so we open accel whenever an accelerator-config is given;
	// the per-sandbox cost is one gRPC dial.
	var (
		accel    *sandbox.AccelRuntime
		accelCfg *sandbox.AcceleratorConfig
	)
	if *accelPath != "" || snapshotKey != "" {
		path := *accelPath
		if path == "" {
			fmt.Fprintln(os.Stderr, "restore: manifest:// requires --accelerator-config")
			return 2
		}
		accelCfg, err = sandbox.LoadAccelerator(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		accel, err = sandbox.OpenAccelRuntime(accelCfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer accel.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()

	exit, err := restore.Run(ctx, restore.Options{
		SnapshotPath:        snapshotPath,
		SnapshotManifestKey: snapshotKey,
		HostCfg:             cfg,
		AccelCfg:            accelCfg,
		AccelRuntime:        accel,
		SandboxID:           *sandboxID,
		CHBinary:            *chBinary,
		RuntimeRoot:         *runtimeRoot,
		StatsJSONPath:       *statsJSON,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}
