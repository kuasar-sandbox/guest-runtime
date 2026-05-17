package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/restore"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
)

// runCmd implements `sandbox-ctl run`. With --restore=<ref> it switches
// to restore mode (snapshot bundle); without it goes cold-start.
//
// stdio model: see docs/sandbox.md §2.2. The app's stdin/stdout/stderr
// (pipe mode) or a single pty (--tty) travel over the vsock stdio MUX.
// --tty defaults to auto-detect (on iff stdin and stdout are both
// terminals); pipe-mode --stdin/--stdout/--stderr and their -from/-to
// variants force pipe mode. The guest kernel dmesg is a separate channel
// — --console off|default|file=PATH (default: our stderr).
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

	// stdio flags. The bool flags (--stdin/--stdout/--stderr/--tty) are
	// tri-state — "not set" must be distinguishable from "set to false"
	// — so we read fs.Visit after Parse to wrap them in *bool.
	stdinFlag := fs.Bool("stdin", false, "pipe mode: connect app stdin to sandbox-ctl's stdin (default off → /dev/null)")
	stdoutFlag := fs.Bool("stdout", true, "pipe mode: app stdout → sandbox-ctl stdout (default on; --stdout=false discards)")
	stderrFlag := fs.Bool("stderr", true, "pipe mode: app stderr → sandbox-ctl stderr (default on; --stderr=false discards)")
	stdinFrom := fs.String("stdin-from", "", "pipe mode: app stdin reads from FILE (implies --stdin)")
	stdoutTo := fs.String("stdout-to", "", "pipe mode: app stdout → FILE (implies --stdout)")
	stderrTo := fs.String("stderr-to", "", "pipe mode: app stderr → FILE (implies --stderr)")
	ttyFlag := fs.Bool("tty", false, "give the app a pty + put our terminal in raw mode (default: auto = on iff stdin&stdout are terminals; mutually exclusive with --stdin/--stdout/--stderr/--*-from/--*-to)")
	console := fs.String("console", "default", "guest kernel dmesg sink: off | default (our stderr) | file=PATH")

	// Reliability backstop: after N consecutive failed pings, sandbox-ctl
	// SIGTERMs CH so cmd.Wait returns rather than hanging on a wedged-
	// but-alive guest. 0 = disabled (default — wait for outer signal).
	// With the default 1 s ping interval, 30 ≈ 30 s of unreachability.
	pingFatal := fs.Int("ping-fatal-threshold", 0,
		"consecutive ping failures before SIGTERMing CH (overrides SANDBOX_PING_FATAL_THRESHOLD env; 0 disables)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --ping-fatal-threshold precedence: flag > env > 0 (disabled).
	pingFatalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ping-fatal-threshold" {
			pingFatalSet = true
		}
	})
	if !pingFatalSet {
		if s := os.Getenv("SANDBOX_PING_FATAL_THRESHOLD"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "sandbox-ctl run: bad SANDBOX_PING_FATAL_THRESHOLD=%q (want non-negative int)\n", s)
				return 2
			}
			*pingFatal = n
		}
	}

	// Detect which tri-state bool flags were explicitly set.
	var stdinSet, stdoutSet, stderrSet, ttySet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stdin":
			stdinSet = true
		case "stdout":
			stdoutSet = true
		case "stderr":
			stderrSet = true
		case "tty":
			ttySet = true
		}
	})
	tri := func(set bool, v *bool) *bool {
		if !set {
			return nil
		}
		x := *v
		return &x
	}
	stdioMode, err := stdio.FromFlags(
		tri(stdinSet, stdinFlag), tri(stdoutSet, stdoutFlag), tri(stderrSet, stderrFlag),
		*stdinFrom, *stdoutTo, *stderrTo, tri(ttySet, ttyFlag), *console)
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

	// Resolve --ch-binary precedence: explicit flag > $SANDBOX_CH_PATH >
	// directory of running sandbox-ctl executable > exec.LookPath.
	chBin := *chBinary
	if chBin == "" {
		chBin, err = locateCH()
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
		if !errors.Is(err, manifest.ErrConfigNotProvided) {
			fmt.Fprintf(os.Stderr, "[sandbox-ctl] manifest config: %v\n", err)
			return 1
		}
		manifestCfg = nil
	}

	// Signal handling lives in pkg/sandbox (lifecycle.go /
	// restore.go) — they own the CH process and forward SIGTERM/SIGINT
	// to it with SIGKILL escalation. So this layer just passes a plain
	// context.
	ctx := context.Background()

	// Restore mode dispatch.
	if *restoreRef != "" {
		return runRestore(ctx, cfg, manifestCfg, *restoreRef,
			*sandboxID, chBin, rd, *statsJSON, stdioMode, *pingFatal)
	}

	exit, err := sandbox.Run(ctx, sandbox.RunOptions{
		Cfg:                cfg,
		ManifestCfg:        manifestCfg,
		SandboxID:          *sandboxID,
		CHBinary:           chBin,
		RuntimeRoot:        rd,
		StatsJSONPath:      *statsJSON,
		StdioMode:          stdioMode,
		PingFatalThreshold: *pingFatal,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}

// runRestore parses the snapshot reference and dispatches to restore.Run.
func runRestore(ctx context.Context, cfg *sandbox.SandboxConfig, manifestCfg *sandbox.ManifestConfig,
	ref, sandboxID, chBin, runDir, statsJSON string, stdioMode stdio.Mode, pingFatal int,
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
		fetcher manifest.FetcherCloser
		err     error
	)
	if manifestCfg != nil {
		fetcher, err = manifestCfg.NewFetcher(manifestCfg.FetchKeyFunc())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer fetcher.Close()
	}

	exit, err := restore.Run(ctx, restore.Options{
		SnapshotPath:        snapshotPath,
		SnapshotManifestKey: snapshotKey,
		HostCfg:             cfg,
		ManifestCfg:         manifestCfg,
		Fetcher:             fetcher,
		SandboxID:           sandboxID,
		CHBinary:            chBin,
		RuntimeRoot:         runDir,
		StatsJSONPath:       statsJSON,
		StdioMode:           stdioMode,
		PingFatalThreshold:  pingFatal,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}

// locateCH resolves the cloud-hypervisor binary with a fixed
// precedence: $SANDBOX_CH_PATH > directory of running sandbox-ctl
// executable > $PATH. The exe-dir fallback covers the release-bundle
// layout where helper binaries ship alongside the tool under
// bin/<arch>/; $PATH covers distro-installed equivalents.
func locateCH() (string, error) {
	if p := os.Getenv("SANDBOX_CH_PATH"); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "cloud-hypervisor")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("cloud-hypervisor"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("cloud-hypervisor: not found (tried $SANDBOX_CH_PATH, exe-dir, $PATH)")
}
