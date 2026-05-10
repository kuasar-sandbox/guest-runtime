package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/memory"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/snapshot"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// RunOptions controls a single sandbox-ctl run invocation.
type RunOptions struct {
	Cfg           *SandboxConfig
	ManifestCfg   *ManifestConfig // for manifest:// resolution; may be nil if all file://
	SandboxID     string          // generated if empty
	CHBinary      string          // path to bin/cloud-hypervisor
	RuntimeRoot   string          // /run prefix; "/run" by default
	StatsJSONPath string          // if set, write vhost stats as JSON to this path on shutdown
	StdioMode     stdio.Mode      // CH process stdio wiring; see pkg/sandbox/stdio
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
	if err := populateSnapshotRefs(opts.Cfg); err != nil {
		return -1, fmt.Errorf("snapshot refs: %w", err)
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

	// Controller handshake (dynamic mode) — must happen before cgroup write
	// so that the controller-granted initial allocatable can override
	// the static startup-burst value when degraded.
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: opts.Cfg.Resources.Control.Controller,
		CHSocket:   chSock,
		CgroupPath: opts.Cfg.Resources.Control.CgroupPath,
		Logf:       logf,
	}, opts.Cfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	if hooks.Enabled() {
		grantedInitial, err := hooks.Admit(opts.SandboxID, 0)
		if err != nil {
			return -1, err
		}
		logf("controller admit ok, initial allocatable=%d", grantedInitial)
	}
	defer hooks.Release("normal")

	// cgroup join. CgroupPath empty → no-cgroup mode, no cgroup operations.
	// CgroupPath set → join existing cgroup (must already exist; not
	// created by sandbox-ctl). See docs/sandbox.md §4.1.
	cgCfg, err := buildCgroupConfig(opts.Cfg)
	if err != nil {
		return -1, err
	}
	// Defer memory.high write to Settled (launch hello). Cold boot's
	// uffd-driven page-fault burst can push the cgroup well past
	// allocatable*0.875; if memory.high is already in effect, every
	// UFFDIO_ZEROPAGE/COPY syscall returns through
	// mem_cgroup_handle_over_high reclaim, throttling the uffd handler
	// against the very faults it's trying to resolve (Issue 4 root cause).
	// memory.max remains the hard ceiling during boot; memory.high gets
	// written by Settled() once the boot transient is past.
	cgCfg.MemoryHighBytes = 0
	cg, err := JoinCgroup(cgCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.joined {
		logf("cgroup joined: %s memory.max=%d memory.high=%d cpu.max=%dus/100000us cpu.weight=%d",
			cg.Path, cgCfg.MemoryMaxBytes, cgCfg.MemoryHighBytes,
			cgCfg.CPUMaxQuotaUs, cgCfg.CPUWeight)
	} else {
		logf("cgroup: no path configured, running without cgroup limits (no-cgroup mode)")
	}
	defer func() { _ = cg.Cleanup() }()

	// Open the accelerator runtime (store + cache clients + crypto)
	// once per sandbox when:
	//   1. any disk URI uses manifest:// (Run / Restore data path), or
	//   2. ManifestCfg points at a store endpoint (lets snapshot --upload
	//      work without re-dialing during the live request).
	// file://-only configs without a manifest config skip the dial.
	var accel *accelRuntime
	wantAccel := needsAccelRuntime(opts.Cfg) ||
		(opts.ManifestCfg != nil && opts.ManifestCfg.Store.Endpoint != "")
	if wantAccel {
		if opts.ManifestCfg == nil {
			return -1, fmt.Errorf("manifest:// disk requires --manifest-config or MANIFEST_CONFIG")
		}
		accel, err = openAccelRuntime(opts.ManifestCfg)
		if err != nil {
			return -1, fmt.Errorf("accelerator runtime: %w", err)
		}
		defer accel.Close()
		logf("accelerator runtime: store=%s cache=%s crypto=%s/%s",
			opts.ManifestCfg.Store.Endpoint, opts.ManifestCfg.Cache.Endpoint,
			opts.ManifestCfg.Crypto.Chunk, opts.ManifestCfg.Crypto.Manifest)
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
		Path:         launchSock,
		Spec:         launchSpec,
		Logf:         logf,
		OnAppStarted: func(pid int) { logf("guest reports user app pid=%d", pid) },
		OnAppExited:  func(code int) { logf("guest reports user app exited code=%d", code) },
	}
	if err := launch.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, err
	}

	// Pinger drives the host→guest health probe (§9.1.4). Started
	// after the launch handshake completes, paused around quiesce, and
	// finally stopped when CH exits.
	pinger := &Pinger{
		Client: &HostClient{BasePath: vsockBase, Logf: logf},
		Stats:  &PingStats{},
		Logf:   logf,
	}

	// ctl.sock server for snapshot requests (P2). The Handler runs in
	// a goroutine spawned by the ctl.sock server; it is allowed to
	// take its time (snapshot path includes /vm.pause + dump + write).
	ctlSockPath := filepath.Join(runDir, "ctl.sock")
	ctlSrv := &snapshot.Server{
		Path: ctlSockPath,
		Logf: logf,
		Handler: func(req snapshot.Request) (snapshot.Response, error) {
			return handleSnapshotRequest(req, opts, memfd, diffPath, srv0, srv1, chSock, runDir, accel, pinger, logf)
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

	// Start the ping ticker as soon as the launch handshake completes
	// (HelloDone = LaunchSpec sent), then gate Settled on the guest's
	// launch_ack — guest has applied the spec, brought up the network,
	// and is about to fork the user app. By that point the boot/launch
	// transient page-fault burst is over, so it is safe to engage
	// memory.high and (in dynamic mode) controller RPCs.
	go func() {
		select {
		case <-launch.HelloDone():
			pinger.Start(backendCtx)
		case <-backendCtx.Done():
			return
		}
		select {
		case <-launch.LaunchAckDone():
			if err := hooks.Settled(); err != nil {
				logf("settled: %v", err)
			}
			if hooks.Enabled() {
				hooks.StartHeartbeat(backendCtx, 5*time.Second)
				hooks.StartSensor(backendCtx, 64<<20)
			}
		case <-backendCtx.Done():
		}
	}()
	defer pinger.Stop()

	_, kernelPath, _ := SchemeAndPath(opts.Cfg.Boot.Kernel)
	_, runtimePath, _ := SchemeAndPath(opts.Cfg.Boot.Runtime)

	args, err := CHCommand(opts.Cfg, blk0Sock, blk1Sock, chSock, vsockBase, kernelPath, runtimePath, uffdSockPath)
	if err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("CH cmdline: %w", err)
	}
	logf("CH args: %s", strings.Join(args, " "))
	cmd := exec.Command(opts.CHBinary, args...)
	stdioCleanup, err := opts.StdioMode.Apply(cmd)
	if err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("stdio: %w", err)
	}
	defer stdioCleanup()
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

	waitErr := waitForCHWithSignalEscalation(doneCh, sigCh, cmd.Process, chPid, chShutdownGrace, logf)
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
		if pinger != nil && pinger.Stats != nil {
			snap := pinger.Stats.Snapshot()
			bundle.Ping = &snap
		}
		if err := writeStatsJSON(opts.StatsJSONPath, bundle); err != nil {
			logf("stats json write %s: %v", opts.StatsJSONPath, err)
		} else {
			logf("stats json written to %s", opts.StatsJSONPath)
		}
	}
	return exit, nil
}

// chShutdownGrace bounds how long we wait for CH to exit cleanly after
// forwarding the first SIGTERM. Cold-target hangs in production observed
// CH not draining the signal for >50 minutes because vCPU was stuck;
// without this escalation, sandbox-ctl waits indefinitely on cmd.Wait.
const chShutdownGrace = 5 * time.Second

// processSignaler is the subset of *os.Process needed by
// waitForCHWithSignalEscalation, exposed for testability.
type processSignaler interface {
	Signal(sig os.Signal) error
}

// waitForCHWithSignalEscalation blocks until doneCh fires, forwarding
// host SIGTERM/SIGINT to the CH process. After the first forward we arm
// a grace deadline; if CH doesn't exit within grace we SIGKILL. A second
// SIGTERM/INT escalates immediately.
func waitForCHWithSignalEscalation(
	doneCh <-chan error,
	sigCh <-chan os.Signal,
	proc processSignaler,
	chPid int,
	grace time.Duration,
	logf func(format string, args ...any),
) error {
	var killTimer *time.Timer
	var killCh <-chan time.Time
	sigtermSent := false
	for {
		select {
		case sig := <-sigCh:
			if !sigtermSent {
				logf("received %v, forwarding SIGTERM to CH (will SIGKILL after %s)", sig, grace)
				_ = proc.Signal(syscall.SIGTERM)
				sigtermSent = true
				killTimer = time.NewTimer(grace)
				killCh = killTimer.C
			} else {
				logf("received %v while shutdown in progress, sending SIGKILL now", sig)
				_ = proc.Signal(syscall.SIGKILL)
				if killTimer != nil {
					killTimer.Stop()
				}
				killCh = nil
			}
		case <-killCh:
			logf("CH didn't exit within %s of SIGTERM, sending SIGKILL pid=%d", grace, chPid)
			_ = proc.Signal(syscall.SIGKILL)
			killCh = nil
		case waitErr := <-doneCh:
			if killTimer != nil {
				killTimer.Stop()
			}
			return waitErr
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
	Cfg         *SandboxConfig
	ManifestCfg *ManifestConfig
	Memfd       *memory.Memfd
	DiffPath string
	Srv0     *vhost.Server
	Srv1     *vhost.Server
	CHSock   string
	RunDir   string
	Accel    *AccelRuntime // wraps internal accelRuntime for cross-package use
	Pinger   *Pinger       // optional; if non-nil, paused around quiesce/Take
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
	opts := RunOptions{Cfg: h.Cfg, ManifestCfg: h.ManifestCfg}
	return handleSnapshotRequest(req, opts, h.Memfd, h.DiffPath, h.Srv0, h.Srv1, h.CHSock, h.RunDir, inner, h.Pinger, h.Logf)
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
	pinger *Pinger,
	logf func(string, ...any),
) (snapshot.Response, error) {
	// Quiesce sequence (§6.2 T2a, §9.1.5): pause ping ticker, ask
	// guest to drain + drop caches, then proceed to /vm.pause via
	// snapshot.Take. Quiesce failure aborts this snapshot rather than
	// degrading dedup; sandbox keeps running.
	if pinger != nil {
		pinger.Pause()
		defer pinger.Resume()
		client := pinger.Client
		if client == nil {
			client = &HostClient{BasePath: filepath.Join(runDir, "vsock.sock"), Logf: logf}
		}
		if err := SendQuiesce(client); err != nil {
			logf("quiesce: %v (aborting snapshot)", err)
			return snapshot.Response{}, fmt.Errorf("quiesce: %w", err)
		}
		logf("quiesce: guest acked, proceeding to /vm.pause")
	}

	if req.Upload && accel == nil {
		return snapshot.Response{}, fmt.Errorf("upload mode requires --manifest-config or MANIFEST_CONFIG (manifest store endpoints)")
	}
	// --output 与 --upload 互斥(docs/sandbox.md §13.3)。CLI 已经做过这道
	// 校验,但 ctl.sock 协议是开放的,run 进程也守在最后一关。
	if req.Upload && req.OutDir != "" {
		return snapshot.Response{}, fmt.Errorf("--output and --upload are mutually exclusive")
	}
	if !req.Upload && req.OutDir == "" {
		return snapshot.Response{}, fmt.Errorf("--output and --upload are mutually exclusive; one is required")
	}
	stagingDir := filepath.Join(runDir, "snap-stage")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return snapshot.Response{}, err
	}
	defer os.RemoveAll(stagingDir)

	// Build snapshot.cfg builder closure. Take() invokes it with the
	// final overlay_ref (file://<sha256>.overlay in --output mode, or
	// the placeholder in --upload mode which Upload() patches afterward
	// with manifest://<key>).
	cfg := opts.Cfg
	snapCfgBuilder := func(overlayRef string) ([]byte, error) {
		return buildSnapshotCfg(cfg, overlayRef)
	}

	takeOutDir := req.OutDir
	sandboxID := opts.SandboxID
	// opts.SandboxID is always populated by the run path (generated when
	// CLI didn't pass one); see Run() in this file.

	src := snapshot.Sources{
		SandboxID:   sandboxID,
		APISock:     chSock,
		MemfdFD:     mfd.FD(),
		MemfdSize:   int64(mfd.Size()),
		DiffPath:    diffPath,
		StagingDir:  stagingDir,
		SnapshotCfg: snapCfgBuilder,
		Quiescer:    &pairQuiescer{a: srv0, b: srv1},
		Logf:        logf,
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
			OverlayPath:      out.OverlayPath,
			OverlayRef:       "file://" + out.OverlaySha256 + ".overlay",
		}, nil
	}

	// Upload mode: ingest blk1.diff (overlay) + snapshot bundle.
	holes, err := snapshot.SparseHoles(out.SnapshotPath, out.MemorySize)
	if err != nil {
		return snapshot.Response{}, fmt.Errorf("scan snapshot holes: %w", err)
	}
	logf("snapshot upload: hole extents=%d (memory section)", len(holes))

	chunkCfg, err := opts.ManifestCfg.BuildChunker()
	if err != nil {
		return snapshot.Response{}, fmt.Errorf("chunker config: %w", err)
	}
	upRes, err := snapshot.Upload(context.Background(), snapshot.UploadSources{
		OverlayPath:    diffPath,
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
		MemorySize:          out.MemorySize,
		MemoryResident:      out.MemoryResident,
		WallclockPauseMs:    out.WallclockPauseMs,
		WallclockDumpMs:     out.WallclockDumpMs,
		SnapshotManifestKey: snapshot.HexKey(upRes.SnapshotKey),
		OverlayManifestKey:  snapshot.HexKey(upRes.OverlayKey),
		OverlayRef:          "manifest://" + snapshot.HexKey(upRes.OverlayKey),
		Msg:                 fmt.Sprintf("upload OK in %d ms; overlay total=%d dedup=%d, snapshot total=%d dedup=%d", upRes.WallclockUploadMs, upRes.OverlayTotalChunks, upRes.OverlayDedupChunks, upRes.SnapshotTotalChunks, upRes.SnapshotDedupChunks),
	}, nil
}

// buildSnapshotCfg renders the snapshot.cfg YAML body per docs §3.4.
// runtime_ref / base_ref are pre-computed by sandbox-ctl at boot
// (file SHA256 is hashed once at startup; see SnapshotRefs in
// SandboxConfig). overlayRef is filled in by Take() after overlay
// digest is known, or by Upload() after overlay manifest key is known.
func buildSnapshotCfg(cfg *SandboxConfig, overlayRef string) ([]byte, error) {
	doc := snapshotCfgYAML{}
	doc.Resources.Capacity.CPU = cfg.Resources.Capacity.CPU
	doc.Resources.Capacity.Memory = cfg.Resources.Capacity.Memory
	doc.Boot.RuntimeRef = cfg.SnapshotRefs.RuntimeRef
	doc.Boot.Root.BaseRef = cfg.SnapshotRefs.BaseRef
	doc.Boot.Root.Overlay.Base = overlayRef
	return yaml.Marshal(&doc)
}

// snapshotCfgYAML mirrors the on-disk snapshot.cfg schema. Extracted
// type so buildSnapshotCfg + applyrules.SnapshotCfg share a definition.
type snapshotCfgYAML struct {
	Resources struct {
		Capacity struct {
			CPU    int    `yaml:"cpu"`
			Memory string `yaml:"memory"`
		} `yaml:"capacity"`
	} `yaml:"resources"`
	Boot struct {
		RuntimeRef string `yaml:"runtime_ref"`
		Root       struct {
			BaseRef string `yaml:"base_ref"`
			Overlay struct {
				Base string `yaml:"base"`
			} `yaml:"overlay"`
		} `yaml:"root"`
	} `yaml:"boot"`
}

// populateSnapshotRefs hashes boot.runtime + boot.root.base (when file://)
// and stores the canonical refs on cfg.SnapshotRefs so buildSnapshotCfg
// can render snapshot.cfg without re-hashing on each request. Called once
// during Run() startup; cost is one streamed read per artifact (typical
// runtime ≈ 5 MiB, base ≈ 100 MiB).
func populateSnapshotRefs(cfg *SandboxConfig) error {
	rRef, err := buildBootRef(cfg.Boot.Runtime, false /* fileOnly=false; runtime is file:// only but caller fields enforce */)
	if err != nil {
		return fmt.Errorf("boot.runtime: %w", err)
	}
	cfg.SnapshotRefs.RuntimeRef = rRef

	if cfg.Boot.Root.Base == "" {
		return nil
	}
	bRef, err := buildBootRef(cfg.Boot.Root.Base, true /* allowManifest */)
	if err != nil {
		return fmt.Errorf("boot.root.base: %w", err)
	}
	cfg.SnapshotRefs.BaseRef = bRef
	return nil
}

// buildBootRef produces the canonical snapshot.cfg ref for a host URL.
//   - file:///abs/path → "file://<basename>@sha256:<hex>"
//   - manifest://<key> → "manifest://<key>" (passthrough; only when
//     allowManifest is true)
func buildBootRef(uri string, allowManifest bool) (string, error) {
	if strings.HasPrefix(uri, "manifest://") {
		if !allowManifest {
			return "", fmt.Errorf("manifest:// not permitted here")
		}
		return uri, nil
	}
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("expected file:// or manifest://, got %q", uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	digest, err := streamFileSha256(path)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.Base(path) + "@sha256:" + digest, nil
}

// streamFileSha256 returns hex(SHA256(file)). Sparse holes read as 0.
func streamFileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

