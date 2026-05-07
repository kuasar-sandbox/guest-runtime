package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/memory"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/snapshot"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// RunOptions controls a single sandbox-ctl run invocation.
type RunOptions struct {
	Cfg           *SandboxConfig
	AccelCfg      *AcceleratorConfig // for manifest:// resolution; may be nil if all file://
	SandboxID     string             // generated if empty
	CHBinary      string             // path to bin/cloud-hypervisor
	RuntimeRoot   string             // /run prefix; "/run" by default
	StatsJSONPath string             // if set, write vhost stats as JSON to this path on shutdown
}

// Run executes one sandbox lifecycle: prepare backends + launch server,
// spawn CH, wait for exit, cleanup. Returns CH's exit code or an error
// if setup failed.
func Run(ctx context.Context, opts RunOptions) (int, error) {
	startUnixNs := time.Now().UnixNano()
	if opts.Cfg == nil {
		return -1, fmt.Errorf("RunOptions.Cfg is nil")
	}
	if opts.SandboxID == "" {
		opts.SandboxID = generateSandboxID()
	}
	if opts.RuntimeRoot == "" {
		opts.RuntimeRoot = "/run"
	}
	if opts.CHBinary == "" {
		opts.CHBinary = "cloud-hypervisor"
	}

	if err := opts.Cfg.ValidateCold(); err != nil {
		return -1, fmt.Errorf("config: %w", err)
	}
	if err := VerifyTAP(opts.Cfg.Network.TAP); err != nil {
		return -1, err
	}

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	defer os.RemoveAll(runDir)

	chSock := filepath.Join(runDir, "ch.sock")
	blk0Sock := filepath.Join(runDir, "blk0.sock")
	blk1Sock := filepath.Join(runDir, "blk1.sock")
	vsockBase := filepath.Join(runDir, "vsock.sock")
	launchSock := fmt.Sprintf("%s_%d", vsockBase, proto.LaunchPort)

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl] "+format, a...) }

	// cgroup setup. Skip on dev hosts without cgroup v2 access.
	allocMemBytes, err := opts.Cfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, err
	}
	cg, err := SetupCgroup(opts.SandboxID,
		opts.Cfg.Resources.Allocatable.CPU,
		int(allocMemBytes>>20))
	if err != nil {
		logf("cgroup setup failed (continuing without): %v", err)
		cg = nil
	}
	defer func() {
		if cg != nil {
			_ = cg.Cleanup()
		}
	}()

	// Open the accelerator runtime (store + cache clients + crypto)
	// once per sandbox when:
	//   1. any disk URI uses manifest:// (Run / Restore data path), or
	//   2. AccelCfg points at a store endpoint (lets snapshot --upload
	//      work without re-dialing during the live request).
	// file://-only configs without an accel-config skip the dial.
	var accel *accelRuntime
	wantAccel := needsAccelRuntime(opts.Cfg) ||
		(opts.AccelCfg != nil && opts.AccelCfg.Store.Endpoint != "")
	if wantAccel {
		if opts.AccelCfg == nil {
			return -1, fmt.Errorf("manifest:// disk requires --accelerator-config")
		}
		accel, err = openAccelRuntime(opts.AccelCfg)
		if err != nil {
			return -1, fmt.Errorf("accelerator runtime: %w", err)
		}
		defer accel.Close()
		logf("accelerator runtime: store=%s cache=%s crypto=%s/%s",
			opts.AccelCfg.Store.Endpoint, opts.AccelCfg.Cache.Endpoint,
			opts.AccelCfg.Crypto.Chunk, opts.AccelCfg.Crypto.Manifest)
	}

	// Resolve disk URIs. blk0 (boot.root.base) is read-only base;
	// blk1 (boot.root.overlay) is the writable layer.
	blk0Reader, _, err := openBlockReader(ctx, opts.Cfg.Boot.Root.Base, accel)
	if err != nil {
		return -1, fmt.Errorf("blk0 base: %w", err)
	}
	defer blk0Reader.Close()

	// Resolve the launch spec for sandbox-init: image config defaults
	// merged with sandbox.yaml `launch:` overrides. Both file:// and
	// manifest:// blk0 produce a vhost.BlockReader that is also a
	// concurrent-safe io.ReaderAt; LoadImageConfigFrom does the ZIP-
	// trailer scan over either source uniformly.
	imageCfg, err := LoadImageConfigFrom(blk0Reader, blk0Reader.Size())
	if err != nil {
		return -1, fmt.Errorf("load rootfs image config: %w", err)
	}
	logf("image config: cmd=%v entrypoint=%v workdir=%q env-keys=%d",
		imageCfg.Cmd, imageCfg.Entrypoint, imageCfg.WorkingDir, len(imageCfg.Env))
	launchSpec, err := MergeLaunch(imageCfg, opts.Cfg.Launch)
	if err != nil {
		return -1, fmt.Errorf("launch spec: %w", err)
	}

	// Network config travels through the same launch handshake. Only
	// populate when the user actually declared an IP — otherwise nil
	// signals "no IP configuration" to sandbox-init.
	if opts.Cfg.Network.IP != "" {
		launchSpec.Network = &proto.NetworkSpec{
			Interface: opts.Cfg.Network.Interface,
			IPCIDR:    opts.Cfg.Network.IP,
			Gateway:   opts.Cfg.Network.Gateway,
			Hostname:  opts.Cfg.Network.Hostname,
		}
		logf("network spec: iface=%s ip=%s gw=%s hostname=%s",
			launchSpec.Network.Interface, launchSpec.Network.IPCIDR,
			launchSpec.Network.Gateway, launchSpec.Network.Hostname)
	}

	var overlayBase vhost.BlockReader
	if opts.Cfg.Boot.Root.Overlay.Base != "" {
		r, _, err := openBlockReader(ctx, opts.Cfg.Boot.Root.Overlay.Base, accel)
		if err != nil {
			return -1, fmt.Errorf("blk1 overlay base: %w", err)
		}
		overlayBase = r
		defer overlayBase.Close()
	}
	_, diffPath, ok := SchemeAndPath(opts.Cfg.Boot.Root.Overlay.Diff)
	if !ok {
		return -1, fmt.Errorf("boot.root.overlay.diff invalid URI: %s", opts.Cfg.Boot.Root.Overlay.Diff)
	}
	overlaySize, err := opts.Cfg.OverlaySize()
	if err != nil {
		return -1, err
	}
	if overlayBase != nil && overlayBase.Size() > overlaySize {
		overlaySize = overlayBase.Size()
	}
	cow, err := vhost.OpenBlockCOW(diffPath, overlayBase, overlaySize)
	if err != nil {
		return -1, fmt.Errorf("overlay COW: %w", err)
	}
	defer cow.Close()

	// Memory setup. sandbox-ctl owns the memfd; CH inherits it via
	// cmd.ExtraFiles[0]=memfd. The uffd is created in CH's process
	// during create_ram_region (mm-binding requires this) and handed
	// back to us via SCM_RIGHTS in the va_report message.
	capBytes, err := opts.Cfg.CapacityMemoryBytes()
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

	uffdSockPath := filepath.Join(runDir, "uffd.sock")

	// AddressMap is shared between the va_report server (which
	// registers ProcessCH on receive) and the uffd Handler (which
	// reads via Locate on each fault).
	addrMap := uffd.NewAddressMap(uint64(memfd.Size()))
	// Backend mmap is a single contiguous VMA covering the entire
	// memfd at offset 0; register it so handleRemove can reciprocal-
	// madvise(DONTNEED, backendVA range) when CH reports EVENT_REMOVE
	// on chVA. CH-side VMAs come in later via va_report.
	if err := addrMap.RegisterVMA(uffd.ProcessBackend, uint64(memfd.Addr()), uint64(memfd.Size()), 0); err != nil {
		return -1, fmt.Errorf("addrmap register backend: %w", err)
	}

	// uffdHandler is constructed inside the va_report OnReady callback
	// once we have CH's uffd fd. Held here so the deferred Close can
	// reach it after ctx cancellation.
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

	vaReportSrv := &uffd.VAReportServer{
		Path:    uffdSockPath,
		AddrMap: addrMap,
		Logf:    logf,
		OnReady: func(uffdFD int, vaStart, size uint64) error {
			h, err := uffd.NewWithBackendUffd(uffdFD, addrMap, uffd.Config{
				MemfdFD:   memfd.FD(),
				BackendVA: memfd.Addr(),
				Size:      memfd.Size(),
				Source:    uffd.ZeroSource{}, // cold-start
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
			// Subsequent regions (PCI-hole-split: low + high). Add the
			// new uffd to the existing handler's epoll set.
			uffdHandlerMu.Lock()
			h := uffdHandler
			uffdHandlerMu.Unlock()
			if h == nil {
				return fmt.Errorf("OnRegister called before OnReady (no handler yet)")
			}
			if err := h.AddUffd(uffdFD); err != nil {
				return err
			}
			logf("uffd handler: attached additional region fd=%d size=%d", uffdFD, size)
			return nil
		},
	}
	if err := vaReportSrv.Listen(); err != nil {
		return -1, fmt.Errorf("va_report listen: %w", err)
	}
	defer vaReportSrv.Stop()

	// Start backends and launch server.
	srv0 := vhost.NewServer(blk0Sock, &vhost.ReadOnlyBackend{R: blk0Reader}, logf)
	srv0.EnableStats("blk0", opts.Cfg.Boot.Root.Base)
	srv0.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv0.Listen(); err != nil {
		return -1, err
	}
	srv1 := vhost.NewServer(blk1Sock, &vhost.CowBackend{C: cow}, logf)
	srv1.EnableStats("blk1", opts.Cfg.Boot.Root.Overlay.Diff)
	srv1.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv1.Listen(); err != nil {
		srv0.Stop()
		return -1, err
	}
	launch := &LaunchServer{
		Path: launchSock,
		Spec: launchSpec,
		Logf: logf,
	}
	if err := launch.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, err
	}

	// ctl.sock server for snapshot requests (P2). The Handler runs in
	// a goroutine spawned by the ctl.sock server; it is allowed to
	// take its time (snapshot path includes /vm.pause + dump + write).
	ctlSockPath := filepath.Join(runDir, "ctl.sock")
	ctlSrv := &snapshot.Server{
		Path: ctlSockPath,
		Logf: logf,
		Handler: func(req snapshot.Request) (snapshot.Response, error) {
			return handleSnapshotRequest(req, opts, memfd, diffPath, srv0, srv1, chSock, runDir, accel, logf)
		},
	}
	if err := ctlSrv.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, fmt.Errorf("ctl.sock listen: %w", err)
	}
	defer ctlSrv.Stop()

	backendCtx, cancelBackends := context.WithCancel(ctx)
	defer cancelBackends()

	var backendWG sync.WaitGroup
	backendWG.Add(5)
	go func() { defer backendWG.Done(); _ = srv0.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = srv1.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = launch.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = vaReportSrv.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = ctlSrv.Serve(backendCtx) }()

	_, kernelPath, _ := SchemeAndPath(opts.Cfg.Boot.Kernel)
	_, runtimePath, _ := SchemeAndPath(opts.Cfg.Boot.Runtime)

	args, err := CHCommand(opts.Cfg, blk0Sock, blk1Sock, chSock, vsockBase, kernelPath, runtimePath, uffdSockPath)
	if err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("CH cmdline: %w", err)
	}
	logf("spawning %s with %d args", opts.CHBinary, len(args))
	logf("CH cmdline: %s %s", opts.CHBinary, joinSpaces(args))
	logf("launch spec: exec=%s args=%v", launchSpec.Exec, launchSpec.Args)

	cmd := exec.Command(opts.CHBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// fd=3 ← memfd in CH (after stdin/out/err). CH creates its own
	// uffd in create_ram_region and hands it back via SCM_RIGHTS;
	// see uffd.VAReportServer.OnReady.
	cmd.ExtraFiles = []*os.File{memfd.File()}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("spawn CH: %w", err)
	}
	chPid := cmd.Process.Pid
	logf("CH started pid=%d", chPid)

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	for {
		select {
		case sig := <-sigCh:
			logf("received %v, forwarding SIGTERM to CH", sig)
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case waitErr := <-doneCh:
			cancelBackends()
			backendWG.Wait()
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
				writeUffdStats(os.Stderr, h.Stats())
			}
			if opts.StatsJSONPath != "" {
				bundle := statsBundle{
					Servers:     []*vhost.Server{srv0, srv1},
					StartUnixNs: startUnixNs,
					EndUnixNs:   time.Now().UnixNano(),
				}
				if h != nil {
					bundle.Uffd = h.Stats()
					if capBytes, err := opts.Cfg.CapacityMemoryBytes(); err == nil {
						bundle.UffdRAMSize = int64(capBytes)
					}
				}
				if err := writeStatsJSON(opts.StatsJSONPath, bundle); err != nil {
					logf("stats json write %s: %v", opts.StatsJSONPath, err)
				} else {
					logf("stats json written to %s", opts.StatsJSONPath)
				}
			}
			return exit, nil
		}
	}
}

// writeUffdStats emits a one-line-per-counter human readable summary
// of uffd handler counters to w. Designed to be quick to eyeball when
// e2e_sandbox_cold dumps stderr.
func writeUffdStats(w io.Writer, s map[string]uint64) {
	fmt.Fprintln(w, "[uffd-stats]")
	keys := []string{
		"faults_absent",
		"faults_released",
		"zeropage_calls",
		"copy_calls",
		"pages_zeroed",
		"pages_copied",
		"wakes",
		"remove_events",
		"remove_q_dropped",
		"remove_events_batched",
		"madvise_calls",
		"madvise_bytes",
		"backend_lookup_miss",
		"errors",
		"batch_calls",
		"batch_pages_total",
		"batch_avg_pages",
		"batch_max_pages",
	}
	for _, k := range keys {
		fmt.Fprintf(w, "  %-20s %d\n", k, s[k])
	}
}

// pairQuiescer drives Quiesce/Resume on both vhost backends together.
type pairQuiescer struct{ a, b *vhost.Server }

func (p *pairQuiescer) Quiesce() {
	p.a.Quiesce()
	p.b.Quiesce()
}
func (p *pairQuiescer) Resume() {
	// LIFO so callers of Quiesce see fully-paused state before any
	// resume kicks the backends.
	p.b.Resume()
	p.a.Resume()
}

// SnapshotHandler bundles the live state needed to service one
// snapshot_request on ctl.sock. Both Run (cold-start) and restore.Run
// build one of these and pass it to ctl.sock listener; lifecycle code
// that owns the bundle holds references; the snapshot path consumes
// them when a request arrives.
type SnapshotHandler struct {
	Cfg      *SandboxConfig
	AccelCfg *AcceleratorConfig
	Memfd    *memory.Memfd
	DiffPath string
	Srv0     *vhost.Server
	Srv1     *vhost.Server
	CHSock   string
	RunDir   string
	Accel    *AccelRuntime // wraps internal accelRuntime for cross-package use
	Logf     func(string, ...any)
}

// Handle dispatches one snapshot.Request. Public for restore.Run; the
// in-package Run calls handleSnapshotRequest directly with its private
// accelRuntime to avoid the wrapper.
func (h *SnapshotHandler) Handle(req snapshot.Request) (snapshot.Response, error) {
	var inner *accelRuntime
	if h.Accel != nil {
		inner = h.Accel.inner
	}
	opts := RunOptions{Cfg: h.Cfg, AccelCfg: h.AccelCfg}
	return handleSnapshotRequest(req, opts, h.Memfd, h.DiffPath, h.Srv0, h.Srv1, h.CHSock, h.RunDir, inner, h.Logf)
}

// handleSnapshotRequest executes one snapshot_request received via
// ctl.sock. Wraps pkg/sandbox/snapshot.Take with the runtime state
// owned by Run.
func handleSnapshotRequest(
	req snapshot.Request,
	opts RunOptions,
	mfd *memory.Memfd,
	diffPath string,
	srv0, srv1 *vhost.Server,
	chSock, runDir string,
	accel *accelRuntime,
	logf func(string, ...any),
) (snapshot.Response, error) {
	if req.Upload && accel == nil {
		return snapshot.Response{}, fmt.Errorf("upload mode requires --accelerator-config (manifest store endpoints)")
	}
	if !req.Upload && req.OutDir == "" {
		return snapshot.Response{}, fmt.Errorf("OutDir required when Upload=false")
	}
	stagingDir := filepath.Join(runDir, "snap-stage")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return snapshot.Response{}, err
	}
	// Local-mode: keep stagingDir for Take to work in but write outputs to req.OutDir.
	// Upload-mode: stagingDir holds disk + bundle, both ingested then dropped.
	if !req.Upload {
		defer os.RemoveAll(stagingDir)
	} else {
		defer os.RemoveAll(stagingDir)
	}

	// Build sandbox.cfg with a placeholder disk reference. Local mode
	// rewrites to "file://disk.ext4" (relative to snapshot file). Upload
	// mode rewrites to "manifest://<disk-key>" *after* the disk is
	// ingested (snapshot.Upload patches the embedded sandbox.cfg).
	cfg := *opts.Cfg
	cfg.Boot.Root.Overlay.Base = "file://disk.ext4"
	sandboxCfg, err := json.Marshal(&cfg)
	if err != nil {
		return snapshot.Response{}, fmt.Errorf("marshal sandbox.cfg: %w", err)
	}

	// Take always produces files in stagingDir/outDir.
	takeOutDir := req.OutDir
	if req.Upload {
		takeOutDir = stagingDir
	}

	src := snapshot.Sources{
		APISock:    chSock,
		MemfdFD:    mfd.FD(),
		MemfdSize:  int64(mfd.Size()),
		DiffPath:   diffPath,
		StagingDir: stagingDir,
		SandboxCfg: sandboxCfg,
		Quiescer:   &pairQuiescer{a: srv0, b: srv1},
		Logf:       logf,
	}
	out, err := snapshot.Take(src, takeOutDir, req.ResumeAfter)
	if err != nil {
		return snapshot.Response{}, err
	}

	if !req.Upload {
		return snapshot.Response{
			MemorySize:       out.MemorySize,
			MemoryResident:   out.MemoryResident,
			WallclockPauseMs: out.WallclockPauseMs,
			WallclockDumpMs:  out.WallclockDumpMs,
			SnapshotPath:     out.SnapshotPath,
			DiskPath:         out.DiskPath,
		}, nil
	}

	// Upload mode: ingest disk + snapshot bundle.
	holes, err := snapshot.SparseHoles(out.SnapshotPath, out.MemorySize)
	if err != nil {
		return snapshot.Response{}, fmt.Errorf("scan snapshot holes: %w", err)
	}
	logf("snapshot upload: hole extents=%d (memory section)", len(holes))

	chunkCfg, err := opts.AccelCfg.BuildChunker()
	if err != nil {
		return snapshot.Response{}, fmt.Errorf("chunker config: %w", err)
	}
	upRes, err := snapshot.Upload(context.Background(), snapshot.UploadSources{
		DiskPath:       out.DiskPath,
		SnapshotPath:   out.SnapshotPath,
		SnapshotHoles:  holes,
		CustomerKey:    accel.customerKey,
		ChunkConfig:    chunkCfg,
		ChunkEncryptor: accel.chunkEnc,
		KTEncryptor:    accel.ktEnc,
		Storer:         accel.store,
		Logf:           logf,
	})
	if err != nil {
		return snapshot.Response{}, err
	}
	return snapshot.Response{
		MemorySize:           out.MemorySize,
		MemoryResident:       out.MemoryResident,
		WallclockPauseMs:     out.WallclockPauseMs,
		WallclockDumpMs:      out.WallclockDumpMs,
		SnapshotManifestKey:  snapshot.HexKey(upRes.SnapshotKey),
		DiskManifestKey:      snapshot.HexKey(upRes.DiskKey),
		Msg:                  fmt.Sprintf("upload OK in %d ms; disk total=%d dedup=%d, snapshot total=%d dedup=%d", upRes.WallclockUploadMs, upRes.DiskTotalChunks, upRes.DiskDedupChunks, upRes.SnapshotTotalChunks, upRes.SnapshotDedupChunks),
	}, nil
}

func generateSandboxID() string {
	b := make([]byte, 4)
	if f, err := os.Open("/dev/urandom"); err == nil {
		defer f.Close()
		_, _ = f.Read(b)
		return fmt.Sprintf("sb-%x", b)
	}
	return "sb-default"
}

func joinSpaces(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
