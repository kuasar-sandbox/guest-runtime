// Package restore implements the `sandbox-ctl run --restore=` lifecycle.
// Reads a <sid>.snapshot bundle (memory + ZIP at end with config.json /
// state.json / snapshot.cfg), prepares memfd + va_report server, spawns
// patched CH with --restore source_url pointing at a temp dir holding
// the rewritten state.json, and lets faults flow.
package restore

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// Options is the restore-specific input.
//
// Snapshot can be supplied either as a local file path or as a
// manifest:// URI. When a manifest:// URI is given, Fetcher must
// be non-nil and the bundle is read via fetch.Fetcher (chunk-granular,
// cache-ctl backed); the uffd source becomes ManifestSnapshotSource
// instead of SparseSnapshotSource.
//
// blk0 / overlay.base in the embedded sandbox.cfg likewise support
// manifest:// when Fetcher is set.
type Options struct {
	SnapshotPath        string                  // file path; mutually exclusive with SnapshotManifestKey
	SnapshotManifestKey string                  // hex content key; mutually exclusive with SnapshotPath
	HostCfg             *sandbox.SandboxConfig  // host yaml: TAP, blk1.diff, etc.
	ManifestCfg         *sandbox.ManifestConfig // for snapshot --upload from a restored sandbox
	Fetcher             fetch.Fetcher           // required when any URI is manifest://; caller owns lifecycle
	SandboxID           string
	CHBinary            string
	RuntimeRoot         string
	StatsJSONPath       string     // if non-empty, dump uffd + per-backend stats here on exit
	StdioMode           stdio.Mode // CH process stdio wiring; see pkg/sandbox/stdio

	// PingFatalThreshold: same semantics as sandbox.RunOptions —
	// SIGTERM CH after N consecutive ping failures. 0 disables.
	PingFatalThreshold int
}

// Run executes restore. Returns the CH exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.SnapshotPath == "" && opts.SnapshotManifestKey == "" {
		return -1, errors.New("restore: SnapshotPath or SnapshotManifestKey required")
	}
	if opts.SnapshotPath != "" && opts.SnapshotManifestKey != "" {
		return -1, errors.New("restore: SnapshotPath and SnapshotManifestKey are mutually exclusive")
	}
	if opts.SnapshotManifestKey != "" && opts.Fetcher == nil {
		return -1, errors.New("restore: manifest:// snapshot requires Fetcher")
	}
	if opts.HostCfg == nil {
		return -1, errors.New("restore: HostCfg required")
	}
	if opts.SandboxID == "" {
		opts.SandboxID = "rs-default"
	}
	if opts.RuntimeRoot == "" {
		opts.RuntimeRoot = "/run"
	}
	if opts.CHBinary == "" {
		opts.CHBinary = "cloud-hypervisor"
	}

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl run --restore] "+format, a...) }
	startUnixNs := time.Now().UnixNano()

	// cgroup join (same semantics as cold-start lifecycle.go). No-cgroup
	// mode (no cgroup_path) is a no-op. See docs/sandbox.md §4.1.
	// Initial memory.high uses the configured allocatable; the value gets
	// bumped after we derive allocatable_at_snapshot from the bundle's
	// state.json balloon (below).
	cg, err := sandbox.JoinCgroupForConfig(opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.Path != "" {
		logf("cgroup joined: %s", cg.Path)
	}
	defer func() { _ = cg.Cleanup() }()

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, err
	}
	defer os.RemoveAll(runDir)

	chSock := filepath.Join(runDir, "ch.sock")
	blk0Sock := filepath.Join(runDir, "blk0.sock")
	blk1Sock := filepath.Join(runDir, "blk1.sock")
	uffdSock := filepath.Join(runDir, "uffd.sock")
	stateDir := filepath.Join(runDir, "snap-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return -1, err
	}

	// Restore-side controller hooks. Admit happens once we've derived
	// allocatable_at_snapshot from the bundle's state.json balloon section
	// (see deriveAllocatableAtSnapshot below).
	// Balloon is created later (snapCap unknown until snapCfg is parsed),
	// then late-injected via hooks.SetBalloon. Until then, hooks balloon-
	// related entry points (SettledRestore, OnAllocatableChanged) treat
	// Balloon-nil as no-op on the balloon side.
	hooks, err := sandbox.NewControllerHooks(sandbox.ControllerHookOptions{
		SocketPath: opts.HostCfg.Resources.Control.Controller,
		CgroupPath: opts.HostCfg.Resources.Control.CgroupPath,
		Logf:       logf,
	}, opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	defer hooks.Release("normal")

	// Source dispatch: file:// → mmap-style local file; manifest:// →
	// fetch.Fetcher random-access via cache-ctl. Both expose the same
	// io.ReaderAt to archive/zip and a corresponding SnapshotReader to
	// the uffd handler.
	var (
		snapFile         *os.File
		snapFetcher      fetch.Stream
		snapReaderAt     io.ReaderAt
		totalSize        int64
		manifestSnapshot bool
	)
	if opts.SnapshotPath != "" {
		f, err := os.OpenFile(opts.SnapshotPath, os.O_RDONLY, 0)
		if err != nil {
			return -1, fmt.Errorf("open snapshot: %w", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return -1, err
		}
		snapFile = f
		snapReaderAt = f
		totalSize = st.Size()
	} else {
		fc, sz, err := sandbox.OpenManifestStream(ctx, opts.SnapshotManifestKey, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open manifest snapshot: %w", err)
		}
		snapFetcher = fc
		snapReaderAt = &fetcherReaderAt{ctx: ctx, fetcher: fc, size: sz}
		totalSize = sz
		manifestSnapshot = true
		logf("manifest snapshot: key=%s bundle_size=%d", opts.SnapshotManifestKey, sz)
	}

	zipReader, err := zip.NewReader(snapReaderAt, totalSize)
	if err != nil {
		return -1, fmt.Errorf("zip.NewReader on snapshot: %w", err)
	}
	entries := make(map[string][]byte)
	for _, f := range zipReader.File {
		rc, err := f.Open()
		if err != nil {
			return -1, fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return -1, fmt.Errorf("zip read %s: %w", f.Name, err)
		}
		entries[f.Name] = body
	}
	for _, want := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := entries[want]; !ok {
			return -1, fmt.Errorf("snapshot bundle missing %s (produced by old sandbox-ctl?)", want)
		}
	}

	// snapshot.cfg carries the post-quiesce platform contract: capacity,
	// runtime_ref, base_ref, overlay.base. ApplyRules merges it with the
	// host sandbox.yaml per docs/sandbox.md §11.0 — capacity must match
	// exactly when host provides it, runtime/base are validated against
	// digest, network.tap is required, overlay.diff is required.
	parsedSnap, err := ParseSnapshotCfg(entries["snapshot.cfg"])
	if err != nil {
		return -1, err
	}
	merged, err := ApplyRules(opts.HostCfg, parsedSnap, opts.SnapshotPath)
	if err != nil {
		return -1, err
	}
	snapCfg := *merged

	// Derive allocatable_at_snapshot from CH state.json's balloon section
	// (no separate resource-state.json file — see §13). When the bundle
	// predates balloon use or balloon was disabled, parseBalloonFromState
	// returns ok=false and we fall back to yaml.allocatable as if it were
	// a cold start.
	snapCap, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("snap sandbox.cfg capacity: %w", err)
	}
	balTarget, balCurrent, balOk, err := parseBalloonFromState(entries["state.json"])
	if err != nil {
		return -1, fmt.Errorf("parse balloon from state.json: %w", err)
	}
	allocAtSnap := deriveAllocatableAtSnapshot(snapCap, balTarget, balCurrent, balOk)

	// BalloonController, sole writer of /vm.resize. Created once we know
	// snapCap and allocAtSnap: target is seeded to `cap - allocAtSnap` so
	// the in-memory state matches what CH will load from state.json when
	// it starts with --restore. Subsequent SettledRestore decides whether
	// a runtime correction is needed (initialAlloc != allocAtSnap).
	var balloonCtl *sandbox.BalloonController
	if allocAtSnap < snapCap {
		balloonCtl = sandbox.NewBalloonController(chSock, snapCap, logf)
		balloonCtl.SetAllocatable(allocAtSnap)
		hooks.SetBalloon(balloonCtl)
	}

	yamlAlloc, err := opts.HostCfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, err
	}

	// Static mode: take max(yaml, snapshot allocatable). When the snapshot
	// was captured under a controller (dynamic mode) at a burst-elevated
	// allocatable, restoring under static mode (A/B) preserves that
	// elevated working set rather than throttling the guest.
	initialAlloc := yamlAlloc
	if allocAtSnap > initialAlloc {
		initialAlloc = allocAtSnap
	}
	if hooks.Enabled() {
		// Dynamic mode: controller decides. Floor sent = yaml.allocatable
		// (controller's 2-tier fallback uses it if headroom can't fit
		// allocAtSnap).
		granted, err := hooks.Admit(opts.SandboxID, allocAtSnap)
		if err != nil {
			return -1, fmt.Errorf("controller admit: %w", err)
		}
		initialAlloc = granted
		logf("controller admit ok, restored allocatable=%d (snapshot allocatable=%d, balloon target/current=%d/%d)",
			granted, allocAtSnap, balTarget, balCurrent)
	} else if allocAtSnap > yamlAlloc {
		logf("static mode: bumping initial allocatable from yaml=%d to snapshot allocatable=%d (balloon target/current=%d/%d)",
			yamlAlloc, allocAtSnap, balTarget, balCurrent)
	}
	// Record initialAlloc in hooks' in-memory state. No external write
	// here: cgroup memory.high is deferred to SettledRestore (Issue 4 —
	// PSI throttling during uffd-driven replay), and balloon already
	// reflects allocAtSnap from the snapshot (any correction needed
	// when initialAlloc != allocAtSnap also happens in SettledRestore,
	// after vm.resume).
	if hooks != nil {
		hooks.SetAllocatableNow(initialAlloc)
	}

	// Resolve disk reference. file:// is opened directly; manifest://
	// goes through pkg/sandbox/disks.OpenManifestFetcher (same path as
	// cold-start manifest:// disks).
	diskRef := snapCfg.Boot.Root.Overlay.Base
	scheme, diskValue, ok := sandbox.SchemeAndPath(diskRef)
	if !ok {
		return -1, fmt.Errorf("invalid overlay.base in sandbox.cfg: %s", diskRef)
	}
	if scheme == "file" && !filepath.IsAbs(diskValue) && opts.SnapshotPath != "" {
		// Resolve relative to snapshot file (local mode only).
		diskValue = filepath.Join(filepath.Dir(opts.SnapshotPath), diskValue)
	}
	if scheme == "manifest" && opts.Fetcher == nil {
		return -1, fmt.Errorf("restore: manifest:// disk in sandbox.cfg requires Fetcher")
	}
	logf("disk image: %s://%s", scheme, diskValue)

	// state.json restored verbatim (vCPU regs, virtio queue indices —
	// nothing path-dependent).
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), entries["state.json"], 0o644); err != nil {
		return -1, err
	}
	// config.json contains paths captured at snapshot time (uffd_socket,
	// vhost_socket, ch.sock api). Rewrite them to point at this run's
	// paths before handing to CH.
	vsockSock := filepath.Join(runDir, "vsock.sock")
	rewritten, err := rewriteConfigPaths(entries["config.json"], pathRewrite{
		UffdSocket: uffdSock,
		Blk0Sock:   blk0Sock,
		Blk1Sock:   blk1Sock,
		APISock:    chSock,
		VsockSock:  vsockSock,
	})
	if err != nil {
		return -1, fmt.Errorf("rewrite config.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), rewritten, 0o644); err != nil {
		return -1, err
	}

	// Memory capacity → memfd size (the memfd itself is owned by
	// sandbox.ServeAndWait). The uffd SnapshotSource is the only
	// restore-specific input to the shared uffd handler: Sparse (file)
	// or Manifest (chunk-granular via cache-ctl) instead of ZeroSource.
	capBytes, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}

	var source uffd.SnapshotReader
	if manifestSnapshot {
		ms, err := uffd.NewManifestSnapshotSource(ctx, snapFetcher, capBytes)
		if err != nil {
			return -1, fmt.Errorf("manifest snapshot source: %w", err)
		}
		source = ms
		logf("snapshot source: manifest:// (chunk-granular fetch via cache-ctl)")
	} else {
		ss, err := uffd.NewSparseSnapshotSource(int(snapFile.Fd()), 0, capBytes)
		if err != nil {
			return -1, fmt.Errorf("snapshot source: %w", err)
		}
		source = ss
		logf("snapshot source: file:// (hole bitmap built)")
	}

	// Open disk as base+diff: disk image (from snapshot) → blk1 base
	// (read-only); new diff at host-side path.
	var baseReader vhost.BlockReader
	switch scheme {
	case "file":
		fr, err := vhost.OpenFileReader(diskValue)
		if err != nil {
			return -1, fmt.Errorf("open disk base: %w", err)
		}
		defer fr.Close()
		baseReader = fr
	case "manifest":
		fc, sz, err := sandbox.OpenManifestStream(ctx, diskValue, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open manifest disk base: %w", err)
		}
		baseReader = vhost.NewManifestReader(ctx, fc, sz)
	default:
		return -1, fmt.Errorf("restore: unknown disk scheme %q", scheme)
	}
	_, diffPath, ok := sandbox.SchemeAndPath(snapCfg.Boot.Root.Overlay.Diff)
	if !ok {
		return -1, fmt.Errorf("bad overlay.diff: %s", snapCfg.Boot.Root.Overlay.Diff)
	}
	overlaySize, err := snapCfg.OverlaySize()
	if err != nil {
		return -1, err
	}
	if baseReader.Size() > overlaySize {
		overlaySize = baseReader.Size()
	}
	cow, err := vhost.OpenBlockCOW(diffPath, baseReader, overlaySize)
	if err != nil {
		return -1, fmt.Errorf("open BlockCOW: %w", err)
	}
	defer cow.Close()

	// blk0 — same rootfs as cold-start (from host yaml or snap.cfg).
	// file:// → mmap; manifest:// → fetch.Fetcher via cache-ctl.
	blk0Path := snapCfg.Boot.Root.Base
	if opts.HostCfg.Boot.Root.Base != "" {
		blk0Path = opts.HostCfg.Boot.Root.Base
	}
	blk0Scheme, blk0Value, ok := sandbox.SchemeAndPath(blk0Path)
	if !ok {
		return -1, fmt.Errorf("invalid blk0 URI: %s", blk0Path)
	}
	var blk0Reader vhost.BlockReader
	switch blk0Scheme {
	case "file":
		fr, err := vhost.OpenFileReader(blk0Value)
		if err != nil {
			return -1, fmt.Errorf("open blk0 (file): %w", err)
		}
		defer fr.Close()
		blk0Reader = fr
	case "manifest":
		if opts.Fetcher == nil {
			return -1, fmt.Errorf("restore: manifest:// blk0 requires Fetcher")
		}
		fc, sz, err := sandbox.OpenManifestStream(ctx, blk0Value, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open blk0 (manifest): %w", err)
		}
		blk0Reader = vhost.NewManifestReader(ctx, fc, sz)
	default:
		return -1, fmt.Errorf("restore: unknown blk0 scheme %q", blk0Scheme)
	}

	// The shared back-half (memfd, uffd va_report handler, vhost-blk
	// backends, the launch server — incl. the guest→host mem_report /
	// app_exited channel that was missing on the restore path — pinger,
	// ctl.sock, signal escalation, stats) lives in sandbox.ServeAndWait.
	// Restore supplies: a snapshot uffd Source (not ZeroSource), a
	// placeholder launch spec (the guest does NOT re-hello after a
	// restore, so WireLaunchMUX=false — the stdio MUX is re-established
	// by PostSpawn over the reverse channel), and a settle protocol of
	// waitAPI → /vm.resume → restore{epoch} → SettledRestore.
	return sandbox.ServeAndWait(sandbox.VMParams{
		Ctx:                ctx,
		SandboxID:          opts.SandboxID,
		RunDir:             runDir,
		Logf:               logf,
		StdioMode:          opts.StdioMode,
		PingFatalThreshold: opts.PingFatalThreshold,
		StartUnixNs:        startUnixNs,
		StatsJSONPath:      opts.StatsJSONPath,

		CapBytes:   int64(capBytes),
		UffdSource: source,
		Blk0Reader: blk0Reader,
		Blk0Label:  "blk0",
		Blk0Path:   blk0Path,
		Cow:        cow,
		Blk1Label:  "blk1",
		Blk1Path:   snapCfg.Boot.Root.Overlay.Diff,

		LaunchSpec:    &proto.LaunchSpec{},
		WireLaunchMUX: false,
		Balloon:       balloonCtl,
		Hooks:         hooks,

		SnapCfg:     &snapCfg,
		ManifestCfg: opts.ManifestCfg,
		DiffPath:    diffPath,

		BuildCmd: func(e sandbox.CmdEnv) (*exec.Cmd, func(), error) {
			// CH 51 `--restore source_url=file://<dir>` replaces
			// --kernel/--vsock; --console/--serial are restored from the
			// snapshot bundle (taken with `--console tty --serial off`),
			// so we don't repeat them. consoleArg is unused here.
			cmd := exec.CommandContext(ctx, opts.CHBinary)
			_, cleanup, err := opts.StdioMode.SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, fmt.Errorf("stdio: %w", err)
			}
			cmd.Args = append(cmd.Args, "--api-socket", e.CHSock, "--restore", "source_url=file://"+stateDir)
			logf("spawning %s --api-socket %s --restore source_url=file://%s",
				opts.CHBinary, e.CHSock, stateDir)
			return cmd, cleanup, nil
		},

		// Restore settle (docs/sandbox.md §7 T14-T15): wait for CH's
		// API, /vm.resume to release the vCPUs from the snapshot point,
		// then notify the guest (restore{epoch=1}) and turn that
		// reverse-channel conn into the stdio MUX. Synchronous — a
		// non-nil return aborts the run (ServeAndWait kills CH); we
		// don't hand back a sandbox whose guest agent is unreachable.
		PostSpawn: func(pc sandbox.PostSpawnCtx) error {
			if err := waitAPI(pc.CHSock, 30*time.Second); err != nil {
				return fmt.Errorf("ch api not ready: %w", err)
			}
			if err := chAPI(pc.CHSock, "PUT", "/api/v1/vm.resume", ""); err != nil {
				return fmt.Errorf("vm.resume: %w", err)
			}
			pc.Logf("VM resumed, vCPU running")

			tRestore := time.Now()
			muxConn, muxSpec, err := sandbox.OpenMUXViaRestore(pc.Pinger.Client, 1, proto.DeadlineRestore)
			if err != nil {
				return fmt.Errorf("notify restore: %w (guest agent unreachable)", err)
			}
			if err := pc.EstablishMUX(muxConn, muxSpec); err != nil {
				return fmt.Errorf("stdio MUX bridge: %w", err)
			}
			pc.Logf("restore notify acked in %dµs (stdio MUX re-established: tty=%v); starting ping ticker",
				time.Since(tRestore).Microseconds(), muxSpec.TTY)
			pc.Pinger.Start(pc.Ctx)
			// Balloon reconcile: idempotent — if initialAlloc ==
			// allocAtSnap, target matches what CH loaded from state.json.
			if pc.Balloon != nil {
				if err := pc.Balloon.Start(pc.Ctx); err != nil {
					pc.Logf("balloon: start: %v", err)
				}
			}
			// Restore-path settled trigger (docs/sandbox.md §10.1):
			// restore_ack is the controller's equivalent of cold-start
			// hello. Writes memory.high (deferred from JoinCgroup —
			// Issue 4) using allocatable_now; corrects balloon only when
			// initialAlloc != allocAtSnap.
			if pc.Hooks != nil {
				if err := pc.Hooks.SettledRestore(allocAtSnap); err != nil {
					pc.Logf("settled-restore: %v (continuing)", err)
				}
				if pc.Hooks.Enabled() {
					pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
					pc.Hooks.StartSensor(pc.Ctx, 64<<20)
				}
			}
			return nil
		},
	})
}

func waitAPI(sock string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("ch api socket not ready")
}

func chAPI(sock, method, path, body string) error {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := c.Write([]byte(req)); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	resp := string(buf[:n])
	if len(resp) < 12 {
		return fmt.Errorf("short response %q", resp)
	}
	status := resp[9:12]
	if status[0] != '2' {
		return fmt.Errorf("non-2xx %s %s: %q", method, path, resp)
	}
	return nil
}
