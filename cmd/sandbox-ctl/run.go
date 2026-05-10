package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fullof-work/mass-sandbox/pkg/binloc"
	"github.com/fullof-work/mass-sandbox/pkg/config"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/restore"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
)

// runCmd implements `sandbox-ctl run`. With --restore=<ref> it switches
// to restore mode (snapshot bundle); without it goes cold-start.
//
// stdio model: see docs/sandbox.md §2.2. By default stdin is closed
// (/dev/null), stdout/stderr inherit sandbox-ctl's; --tty allocates a
// pty and bridges to the controlling terminal; per-stream --xxx /
// --xxx=false / --xxx-from FILE / --xxx-to FILE override.
//
// --ch-binary defaults to a precedence chain: SANDBOX_CH_PATH env →
// directory of running sandbox-ctl executable → exec.LookPath.
//
// --run-dir defaults to SANDBOX_RUN_DIR env or "/run".
func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)

	configPath := fs.String("config", "", "path to sandbox.yaml (or SANDBOX_CONFIG env)")
	manifestPath := fs.String("manifest-config", "", "path to manifest config YAML (overrides MANIFEST_CONFIG env; required for manifest:// resources)")
	sandboxID := fs.String("sandbox-id", "", "sandbox id (overrides sandbox.yaml)")
	chBinary := fs.String("ch-binary", "", "path to cloud-hypervisor binary (default: SANDBOX_CH_PATH env, exe-dir, or PATH)")
	runDir := fs.String("run-dir", "", "host runtime state dir (overrides SANDBOX_RUN_DIR env; default /run)")

	cgroupPath := fs.String("cgroup-path", "", "absolute cgroup v2 directory to join (must already exist); empty = no cgroup")
	statsJSON := fs.String("stats-json", "", "if set, write per-backend + uffd stats as JSON to this path on shutdown")

	restoreRef := fs.String("restore", "", "snapshot reference (file path or manifest://<hex>) — switches to restore mode")

	// stdio flags. We use *bool wrappers via Var() since flag.Bool
	// can't distinguish "not set" from "set to false". Workaround: parse
	// raw args first to detect presence.
	stdinFlag := fs.Bool("stdin", false, "enable sandbox stdin (default off)")
	stdoutFlag := fs.Bool("stdout", true, "enable sandbox stdout (default on)")
	stderrFlag := fs.Bool("stderr", true, "enable sandbox stderr (default on)")
	stdinFrom := fs.String("stdin-from", "", "redirect sandbox stdin from FILE")
	stdoutTo := fs.String("stdout-to", "", "redirect sandbox stdout to FILE")
	stderrTo := fs.String("stderr-to", "", "redirect sandbox stderr to FILE")
	tty := fs.Bool("tty", false, "allocate pty + bridge to controlling tty (mutually exclusive with --stdin/--stdout/--stderr/--*-from/--*-to)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Detect which stdio bool flags were explicitly set so stdio.FromFlags
	// can distinguish "default" from "explicit false".
	var stdinSet, stdoutSet, stderrSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stdin":
			stdinSet = true
		case "stdout":
			stdoutSet = true
		case "stderr":
			stderrSet = true
		}
	})
	var stdinPtr, stdoutPtr, stderrPtr *bool
	if stdinSet {
		v := *stdinFlag
		stdinPtr = &v
	}
	if stdoutSet {
		v := *stdoutFlag
		stdoutPtr = &v
	}
	if stderrSet {
		v := *stderrFlag
		stderrPtr = &v
	}

	stdioMode, err := stdio.FromFlags(stdinPtr, stdoutPtr, stderrPtr,
		*stdinFrom, *stdoutTo, *stderrTo, *tty)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-ctl run: %v\n", err)
		return 2
	}

	// Resolve --config (flag > env).
	if *configPath == "" {
		*configPath = os.Getenv("SANDBOX_CONFIG")
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl run: --config or SANDBOX_CONFIG required")
		return 2
	}
	cfg, err := sandbox.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *cgroupPath != "" {
		cfg.Resources.Control.CgroupPath = *cgroupPath
	}

	// Resolve --ch-binary precedence: explicit flag > binloc lookup.
	chBin := *chBinary
	if chBin == "" {
		chBin, err = binloc.Locate("cloud-hypervisor", "SANDBOX_CH_PATH")
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-ctl run: cloud-hypervisor not found: %v\n", err)
			return 1
		}
	}

	// Resolve --run-dir precedence: flag > env > /run.
	rd := *runDir
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_DIR")
	}
	if rd == "" {
		rd = "/run"
	}

	// Optional manifest config (required for manifest:// resources).
	manifestCfg, err := sandbox.LoadManifestConfig(*manifestPath)
	if err != nil {
		if !errors.Is(err, config.ErrConfigNotProvided) {
			fmt.Fprintf(os.Stderr, "[sandbox-ctl] manifest config: %v\n", err)
			return 1
		}
		manifestCfg = nil
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Restore mode dispatch.
	if *restoreRef != "" {
		return runRestore(ctx, cfg, manifestCfg, *restoreRef,
			*sandboxID, chBin, rd, *statsJSON, stdioMode)
	}

	exit, err := sandbox.Run(ctx, sandbox.RunOptions{
		Cfg:           cfg,
		ManifestCfg:   manifestCfg,
		SandboxID:     *sandboxID,
		CHBinary:      chBin,
		RuntimeRoot:   rd,
		StatsJSONPath: *statsJSON,
		StdioMode:     stdioMode,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}

// runRestore parses the snapshot reference and dispatches to restore.Run.
func runRestore(ctx context.Context, cfg *sandbox.SandboxConfig, manifestCfg *sandbox.ManifestConfig,
	ref, sandboxID, chBin, runDir, statsJSON string, stdioMode stdio.Mode,
) int {
	const manifestPrefix = "manifest://"
	var (
		snapshotPath string
		snapshotKey  string
	)
	if strings.HasPrefix(ref, manifestPrefix) {
		snapshotKey = strings.TrimPrefix(ref, manifestPrefix)
		if manifestCfg == nil {
			fmt.Fprintln(os.Stderr, "sandbox-ctl run --restore=manifest://: requires --manifest-config or MANIFEST_CONFIG")
			return 2
		}
	} else {
		snapshotPath = ref
	}

	var (
		accel *sandbox.AccelRuntime
		err   error
	)
	if manifestCfg != nil {
		accel, err = sandbox.OpenAccelRuntime(manifestCfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer accel.Close()
	}

	exit, err := restore.Run(ctx, restore.Options{
		SnapshotPath:        snapshotPath,
		SnapshotManifestKey: snapshotKey,
		HostCfg:             cfg,
		ManifestCfg:         manifestCfg,
		AccelRuntime:        accel,
		SandboxID:           sandboxID,
		CHBinary:            chBin,
		RuntimeRoot:         runDir,
		StatsJSONPath:       statsJSON,
		StdioMode:           stdioMode,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}
