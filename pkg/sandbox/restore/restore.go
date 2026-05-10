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
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/memory"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/snapshot"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// Options is the restore-specific input.
//
// Snapshot can be supplied either as a local file path or as a
// manifest:// URI. When a manifest:// URI is given, AccelRuntime must
// be non-nil and the bundle is read via fetch.Fetcher (chunk-granular,
// cache-ctl backed); the uffd source becomes ManifestSnapshotSource
// instead of SparseSnapshotSource.
//
// blk0 / overlay.base in the embedded sandbox.cfg likewise support
// manifest:// when AccelRuntime is set.
type Options struct {
	SnapshotPath        string                  // file path; mutually exclusive with SnapshotManifestKey
	SnapshotManifestKey string                  // hex content key; mutually exclusive with SnapshotPath
	HostCfg             *sandbox.SandboxConfig  // host yaml: TAP, blk1.diff, etc.
	ManifestCfg         *sandbox.ManifestConfig // for snapshot --upload from a restored sandbox
	AccelRuntime        *sandbox.AccelRuntime   // required when any URI is manifest://
	SandboxID           string
	CHBinary            string
	RuntimeRoot         string
	StatsJSONPath       string     // if non-empty, dump uffd + per-backend stats here on exit
	StdioMode           stdio.Mode // CH process stdio wiring; see pkg/sandbox/stdio
}

// Run executes restore. Returns the CH exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.SnapshotPath == "" && opts.SnapshotManifestKey == "" {
		return -1, errors.New("restore: SnapshotPath or SnapshotManifestKey required")
	}
	if opts.SnapshotPath != "" && opts.SnapshotManifestKey != "" {
		return -1, errors.New("restore: SnapshotPath and SnapshotManifestKey are mutually exclusive")
	}
	if opts.SnapshotManifestKey != "" && opts.AccelRuntime == nil {
		return -1, errors.New("restore: manifest:// snapshot requires AccelRuntime")
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
	hooks, err := sandbox.NewControllerHooks(sandbox.ControllerHookOptions{
		SocketPath: opts.HostCfg.Resources.Control.Controller,
		CHSocket:   chSock,
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
		snapFetcher      *fetch.Fetcher
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
		fc, sz, err := sandbox.OpenManifestFetcher(ctx, opts.SnapshotManifestKey, opts.AccelRuntime)
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
	// Re-apply cgroup memory.high and (later) balloon target to match
	// initialAlloc. Balloon is configured via vm.resize after /vm.resume
	// because restore loads its initial balloon size from state.json.
	if hooks != nil {
		if err := hooks.ApplyInitialAllocatable(initialAlloc); err != nil {
			logf("apply initial allocatable: %v (continuing)", err)
		}
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
	if scheme == "manifest" && opts.AccelRuntime == nil {
		return -1, fmt.Errorf("restore: manifest:// disk in sandbox.cfg requires AccelRuntime")
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

	// Memory + uffd setup using the SAME path as cold-start, but
	// SnapshotSource is SparseSnapshotSource pointing at the memory
	// section [0, ramSize) of the snapshot file.
	capBytes, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}
	memfd, err := memory.Create("sandbox-"+opts.SandboxID+"-ram", int64(capBytes))
	if err != nil {
		return -1, fmt.Errorf("memfd create: %w", err)
	}
	defer memfd.Close()
	logf("memfd ready: inode=%d size=%d backendVA=0x%x",
		memfd.Inode(), memfd.Size(), memfd.Addr())

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

	addrMap := uffd.NewAddressMap(uint64(memfd.Size()))
	if err := addrMap.RegisterVMA(uffd.ProcessBackend, uint64(memfd.Addr()), uint64(memfd.Size()), 0); err != nil {
		return -1, fmt.Errorf("addrmap register backend: %w", err)
	}
	var uffdHandler *uffd.Handler
	var uffdHandlerMu sync.Mutex
	defer func() {
		uffdHandlerMu.Lock()
		h := uffdHandler
		uffdHandlerMu.Unlock()
		if h != nil {
			_ = h.Close()
		}
	}()

	vaSrv := &uffd.VAReportServer{
		Path:    uffdSock,
		AddrMap: addrMap,
		Logf:    logf,
		OnReady: func(uffdFD int, vaStart, size uint64) error {
			h, err := uffd.NewWithBackendUffd(uffdFD, addrMap, uffd.Config{
				MemfdFD:   memfd.FD(),
				BackendVA: memfd.Addr(),
				Size:      memfd.Size(),
				Source:    source,
				Logf:      logf,
			})
			if err != nil {
				return err
			}
			h.Start()
			uffdHandlerMu.Lock()
			uffdHandler = h
			uffdHandlerMu.Unlock()
			logf("uffd handler: adopted CH uffd region #0 fd=%d size=%d", uffdFD, size)
			return nil
		},
		OnRegister: func(uffdFD int, vaStart, size uint64) error {
			uffdHandlerMu.Lock()
			h := uffdHandler
			uffdHandlerMu.Unlock()
			if h == nil {
				return fmt.Errorf("OnRegister called before OnReady")
			}
			if err := h.AddUffd(uffdFD); err != nil {
				return err
			}
			logf("uffd handler: attached additional region fd=%d size=%d", uffdFD, size)
			return nil
		},
	}
	if err := vaSrv.Listen(); err != nil {
		return -1, fmt.Errorf("va_report listen: %w", err)
	}
	defer vaSrv.Stop()

	// Open disk as base+diff:
	// - disk image (from snapshot) → blk1 base (read-only)
	// - new diff at host-side path
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
		fc, sz, err := sandbox.OpenManifestFetcher(ctx, diskValue, opts.AccelRuntime)
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
	// Same scheme dispatch as overlay base above: file:// → mmap;
	// manifest:// → fetch.Fetcher via cache-ctl. The OpenFileReader-
	// only path was a v1 leftover that broke as soon as snapshots
	// got uploaded with manifest:// blk0.
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
		if opts.AccelRuntime == nil {
			return -1, fmt.Errorf("restore: manifest:// blk0 requires AccelRuntime")
		}
		fc, sz, err := sandbox.OpenManifestFetcher(ctx, blk0Value, opts.AccelRuntime)
		if err != nil {
			return -1, fmt.Errorf("open blk0 (manifest): %w", err)
		}
		blk0Reader = vhost.NewManifestReader(ctx, fc, sz)
	default:
		return -1, fmt.Errorf("restore: unknown blk0 scheme %q", blk0Scheme)
	}

	srv0 := vhost.NewServer(blk0Sock, &vhost.ReadOnlyBackend{R: blk0Reader}, logf)
	srv0.EnableStats("blk0", blk0Path)
	srv0.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv0.Listen(); err != nil {
		return -1, err
	}
	srv1 := vhost.NewServer(blk1Sock, &vhost.CowBackend{C: cow}, logf)
	srv1.EnableStats("blk1", snapCfg.Boot.Root.Overlay.Diff)
	srv1.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv1.Listen(); err != nil {
		srv0.Stop()
		return -1, err
	}

	// Pinger drives the host→guest health probe across the restored
	// sandbox lifetime (§9.1.4). Started after the restore notification
	// is acked; paused around any subsequent snapshot quiesce window.
	pinger := &sandbox.Pinger{
		Client: &sandbox.HostClient{BasePath: vsockSock, Logf: logf},
		Stats:  &sandbox.PingStats{},
		Logf:   logf,
	}
	defer pinger.Stop()

	// ctl.sock server — same protocol as Run, lets `sandbox-ctl
	// snapshot --sandbox-id <sid>` work against a restored sandbox.
	ctlSockPath := filepath.Join(runDir, "ctl.sock")
	snapHandler := &sandbox.SnapshotHandler{
		Cfg:         &snapCfg,
		ManifestCfg: nil, // restore.Run owns AccelRuntime via opts; ManifestCfg only needed for ChunkConfig
		Memfd:       memfd,
		DiffPath:    diffPath,
		Srv0:        srv0,
		Srv1:        srv1,
		CHSock:      chSock,
		RunDir:      runDir,
		Accel:       opts.AccelRuntime,
		Pinger:      pinger,
		Logf:        logf,
	}
	// ManifestCfg is needed for chunker config in upload mode. The host
	// passes it via the option; if absent, --upload from a restored
	// sandbox will fail with a clear message.
	if opts.ManifestCfg != nil {
		snapHandler.ManifestCfg = opts.ManifestCfg
	}
	ctlSrv := &snapshot.Server{
		Path:    ctlSockPath,
		Logf:    logf,
		Handler: snapHandler.Handle,
	}
	if err := ctlSrv.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, fmt.Errorf("ctl.sock listen: %w", err)
	}
	defer ctlSrv.Stop()

	backendCtx, cancelBackends := context.WithCancel(ctx)
	defer cancelBackends()
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); _ = srv0.Serve(backendCtx) }()
	go func() { defer wg.Done(); _ = srv1.Serve(backendCtx) }()
	go func() { defer wg.Done(); _ = vaSrv.Serve(backendCtx) }()
	go func() { defer wg.Done(); _ = ctlSrv.Serve(backendCtx) }()

	// Build CH cmdline for restore. CH 51 supports
	// `--restore source_url=file://<dir>` as a separate flag from
	// --kernel; we use it instead of --kernel/--vsock.
	args := []string{
		"--api-socket", chSock,
		"--restore", "source_url=file://" + stateDir,
	}
	logf("spawning %s --api-socket %s --restore source_url=file://%s",
		opts.CHBinary, chSock, stateDir)
	cmd := exec.CommandContext(ctx, opts.CHBinary, args...)
	stdioCleanup, err := opts.StdioMode.Apply(cmd)
	if err != nil {
		return -1, fmt.Errorf("stdio: %w", err)
	}
	defer stdioCleanup()
	cmd.ExtraFiles = []*os.File{memfd.File()}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		cancelBackends()
		wg.Wait()
		return -1, fmt.Errorf("spawn CH: %w", err)
	}
	logf("CH started pid=%d", cmd.Process.Pid)

	// CH /vm.restore needs a preceding /vm.create — actually restore
	// flow expects: spawn → /vm.create with restore section → /vm.boot
	// or auto. Wait for api socket to be ready.
	if err := waitAPI(chSock, 30*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return -1, fmt.Errorf("ch api not ready: %w", err)
	}
	// `--restore source_url=...` on the CLI causes CH to auto-load
	// the snapshot bundle into a paused VM at startup. We just need
	// /vm.resume to release the vCPUs.
	if err := chAPI(chSock, "PUT", "/api/v1/vm.resume", ""); err != nil {
		_ = cmd.Process.Kill()
		return -1, fmt.Errorf("vm.resume: %w", err)
	}
	logf("VM resumed, vCPU running")
	startUnixNs := time.Now().UnixNano()

	// Notify guest agent of the restore (§7.1 T13a). The guest's
	// reverse-channel listener was preserved across the snapshot
	// (§9.1.5), so the first dial after vm.resume should land in
	// kernel-microseconds. Failure here aborts restore — we don't
	// want to hand back a sandbox whose guest agent is unreachable.
	tRestore := time.Now()
	if err := sandbox.SendRestore(pinger.Client, 1); err != nil {
		_ = cmd.Process.Kill()
		return -1, fmt.Errorf("notify restore: %w (guest agent unreachable)", err)
	}
	logf("restore notify acked in %dµs; starting ping ticker",
		time.Since(tRestore).Microseconds())
	pinger.Start(backendCtx)
	// Restore-path settled trigger (docs/sandbox.md §10.1):
	// SendRestore returning nil means guest replied `restored` ack,
	// equivalent to cold-start `hello` from the controller's POV. Unlike
	// cold start we do NOT shrink balloon — restored allocatable is
	// preserved as-is. SettledRestore writes memory.high in BOTH static
	// and dynamic modes (deferred from JoinCgroup; Issue 4 root cause).
	if hooks != nil {
		if err := hooks.SettledRestore(); err != nil {
			logf("settled-restore: %v (continuing)", err)
		}
		if hooks.Enabled() {
			hooks.StartHeartbeat(backendCtx, 5*time.Second)
			hooks.StartSensor(backendCtx, 64<<20)
		}
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigCh:
			logf("received %v, signalling CH", sig)
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case waitErr := <-doneCh:
			cancelBackends()
			wg.Wait()
			exit := 0
			if waitErr != nil {
				if exitErr, ok := waitErr.(*exec.ExitError); ok {
					exit = exitErr.ExitCode()
				} else {
					return -1, fmt.Errorf("CH wait: %w", waitErr)
				}
			}
			logf("CH exited code=%d", exit)
			_, _ = srv0.WriteStatsTo(os.Stderr)
			_, _ = srv1.WriteStatsTo(os.Stderr)
			uffdHandlerMu.Lock()
			h := uffdHandler
			uffdHandlerMu.Unlock()
			if h != nil {
				dumpUffdStats(os.Stderr, h.Stats())
			}
			if opts.StatsJSONPath != "" {
				bundle := sandbox.StatsBundle{
					Servers:     []*vhost.Server{srv0, srv1},
					StartUnixNs: startUnixNs,
					EndUnixNs:   time.Now().UnixNano(),
				}
				if h != nil {
					bundle.Uffd = h.Stats()
					if cap, err := snapCfg.CapacityMemoryBytes(); err == nil {
						bundle.UffdRAMSize = int64(cap)
					}
				}
				if pinger.Stats != nil {
					snap := pinger.Stats.Snapshot()
					bundle.Ping = &snap
				}
				if err := sandbox.WriteStatsJSON(opts.StatsJSONPath, bundle); err != nil {
					logf("stats json write %s: %v", opts.StatsJSONPath, err)
				} else {
					logf("stats json written to %s", opts.StatsJSONPath)
				}
			}
			return exit, nil
		}
	}
}

func dumpUffdStats(w io.Writer, s map[string]uint64) {
	fmt.Fprintln(w, "[uffd-stats]")
	keys := []string{
		"faults_absent", "faults_released",
		"zeropage_calls", "copy_calls",
		"pages_zeroed", "pages_copied",
		"wakes", "remove_events", "remove_q_dropped", "remove_events_batched",
		"madvise_calls", "madvise_bytes", "backend_lookup_miss", "errors",
		"batch_calls", "batch_pages_total", "batch_avg_pages", "batch_max_pages",
	}
	for _, k := range keys {
		fmt.Fprintf(w, "  %-20s %d\n", k, s[k])
	}
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
