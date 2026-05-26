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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/ctl"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/memory"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/snapshot"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/tapfd"
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

	// PingFatalThreshold: after this many consecutive ping failures
	// the host SIGTERMs CH so cmd.Wait() returns. 0 = disabled
	// (default — wait for the user's signal). With the default 1 s
	// ping interval, 30 ≈ 30 s of unreachability. See pinger.go.
	PingFatalThreshold int
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
	// tap-name mode: CH opens the host tap, so verify it exists now.
	// tapfd mode: the fd comes from the provider handoff below (no host tap
	// to verify here).
	if opts.Cfg.Network.TAP != "" {
		if err := VerifyTAP(opts.Cfg.Network.TAP); err != nil {
			return -1, err
		}
	}
	if err := populateSnapshotRefs(opts.Cfg); err != nil {
		return -1, fmt.Errorf("snapshot refs: %w", err)
	}

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	defer os.RemoveAll(runDir)

	// chSock is needed here (front-half) because BalloonController is
	// constructed before ServeAndWait; the remaining socket paths are
	// owned by ServeAndWait, which derives them identically from runDir.
	chSock := filepath.Join(runDir, "ch.sock")

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl] "+format, a...) }

	// Resolve capacity / floor up front so we can build the BalloonController
	// before hooks (hooks owns "alloc change → balloon target" routing,
	// which needs balloonCtl in hand). Both are cheap yaml lookups; the
	// memfd-creation block below reuses capBytes.
	capBytes, err := opts.Cfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}
	allocBytes, _ := opts.Cfg.AllocatableMemoryBytes()

	// BalloonController is the sole writer of /api/v1/vm.resize. Created
	// only when alloc < cap (no balloon device when alloc == cap).
	// SetAllocatable(allocBytes) here matches the --balloon size= value
	// passed to CH in ch.go, so the in-memory target agrees with CH from
	// the start without an extra round-trip after launch.
	var balloonCtl *BalloonController
	if allocBytes < capBytes {
		balloonCtl = NewBalloonController(chSock, capBytes, logf)
		balloonCtl.SetAllocatable(allocBytes)
	}

	// Controller handshake (dynamic mode) — must happen before cgroup write
	// so that the controller-granted initial allocatable can override
	// the static startup-burst value when degraded. Balloon is injected
	// here so any later OnAllocatableChanged / SettledRestore call routes
	// through balloonCtl rather than opening its own HTTP path.
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: opts.Cfg.Resources.Control.Controller,
		CgroupPath: opts.Cfg.Resources.Control.CgroupPath,
		Logf:       logf,
		Balloon:    balloonCtl,
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

	// Open the read-side fetcher (cache + store clients + decryptor)
	// once per sandbox when any disk URI uses manifest://. file://-only
	// configs without a manifest config skip the dial. snapshot --upload
	// builds its own (write-side) Ingester at request time; the two
	// paths never share a connection pool.
	var fetcher manifest.FetcherCloser
	if needsManifestFetcher(opts.Cfg) {
		if opts.ManifestCfg == nil {
			return -1, fmt.Errorf("manifest:// disk requires --manifest-config or MANIFEST_CONFIG")
		}
		fetcher, err = opts.ManifestCfg.NewFetcher(opts.ManifestCfg.FetchKeyFunc())
		if err != nil {
			return -1, fmt.Errorf("manifest fetcher: %w", err)
		}
		defer fetcher.Close()
		logf("manifest fetcher: store=%s cache=%s crypto=%s/%s",
			opts.ManifestCfg.Store.Endpoint, opts.ManifestCfg.Cache.Endpoint,
			opts.ManifestCfg.Crypto.Chunk, opts.ManifestCfg.Crypto.Manifest)
	}

	// Resolve disk URIs. blk0 (boot.root.base) is read-only base;
	// blk1 (boot.root.overlay) is the writable layer.
	blk0Reader, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Base, fetcher)
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

	// Network acquisition. tapfd mode (docs/tapfd.md §5) execs the provider
	// helper to receive a tap queue fd + metadata; tap-name mode was verified
	// above and CH opens it. The handoff metadata overrides the static attrs
	// (mac/ip/mtu). The resolved spec travels through the launch handshake;
	// nil → "no IP configuration" to sandbox-init.
	var tapFile, netnsFile *os.File
	var metaMAC, metaIP string
	var metaMTU int
	if opts.Cfg.Network.TapFD != nil {
		argv, err := opts.Cfg.Network.TapFD.ResolvedExec()
		if err != nil {
			return -1, err
		}
		f, nsf, meta, err := tapfd.Acquire(ctx, argv, opts.Cfg.Network.TapFD.TimeoutDuration())
		if err != nil {
			return -1, fmt.Errorf("tapfd handoff: %w", err)
		}
		tapFile = f
		defer tapFile.Close()
		netnsFile = nsf // non-nil only if the provider's tap is netns-isolated
		if netnsFile != nil {
			defer netnsFile.Close()
		}
		metaMAC, metaIP, metaMTU = meta.MAC, meta.IP, meta.MTU
		logf("tapfd: received tap fd (mac=%s ip=%s mtu=%d netns=%t)", meta.MAC, meta.IP, meta.MTU, netnsFile != nil)
	}
	netMAC, netSpec := opts.Cfg.Network.Effective(metaMAC, metaIP, metaMTU)
	launchSpec.Network = netSpec

	// Environment setup carried in the launch spec (applied guest-side
	// before the app forks): mounts (incl. image Volumes → empty mounts),
	// injected files, one-shot init, and the shutdown grace.
	launchSpec.Mounts = effectiveMounts(opts.Cfg.Mounts, imageCfg.Volumes)
	launchSpec.Files = toProtoFiles(opts.Cfg.Files)
	launchSpec.Init = toProtoInit(opts.Cfg.Init)
	launchSpec.StopGraceSec = opts.Cfg.StopGraceSeconds()

	// App stdio: tell sandbox-init what to wire (pty vs pipe channels);
	// the launch-handshake connection becomes the stdio MUX after
	// launch_ack (LaunchServer.OnMUXReady below).
	launchSpec.Stdio = opts.StdioMode.ProtoSpec()
	if cols, rows, ok := opts.StdioMode.InitialWinsize(); ok {
		launchSpec.Stdio.Winsize = &proto.Winsize{Cols: cols, Rows: rows}
	}

	var overlayBase vhost.BlockReader
	if opts.Cfg.Boot.Root.Overlay.Base != "" {
		r, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Overlay.Base, fetcher)
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

	// The shared back-half (memfd, uffd va_report handler, vhost-blk
	// backends, launch server, pinger, ctl.sock, signal escalation,
	// stats) lives in ServeAndWait. Cold start supplies: ZeroSource
	// faults, the merged launch spec whose conn becomes the stdio MUX
	// (WireLaunchMUX), and a settle protocol gated on the guest's
	// hello / launch_ack handshake.
	return ServeAndWait(VMParams{
		Ctx:                ctx,
		SandboxID:          opts.SandboxID,
		RunDir:             runDir,
		Logf:               logf,
		StdioMode:          opts.StdioMode,
		PingFatalThreshold: opts.PingFatalThreshold,
		StartUnixNs:        startUnixNs,
		StatsJSONPath:      opts.StatsJSONPath,

		CapBytes:   int64(capBytes),
		UffdSource: uffd.ZeroSource{},
		Blk0Reader: blk0Reader,
		Blk0Label:  "blk0",
		Blk0Path:   opts.Cfg.Boot.Root.Base,
		Cow:        cow,
		Blk1Label:  "blk1",
		Blk1Path:   opts.Cfg.Boot.Root.Overlay.Diff,

		LaunchSpec:    launchSpec,
		WireLaunchMUX: true,
		StartTimeout:  opts.Cfg.StartTimeoutDuration(),
		Balloon:       balloonCtl,
		Hooks:         hooks,

		TapFile:   tapFile, // nil in tap-name mode; CH inherits it at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:     opts.Cfg,
		ManifestCfg: opts.ManifestCfg,
		DiffPath:    diffPath,

		BuildCmd: func(e CmdEnv) (*exec.Cmd, func(), error) {
			_, kernelPath, _ := SchemeAndPath(opts.Cfg.Boot.Kernel)
			_, runtimePath, _ := SchemeAndPath(opts.Cfg.Boot.Runtime)
			// CH process stdio (docs/sandbox.md §5.2): stdin = /dev/null
			// (so CH's `--console tty` never raw-izes a terminal), stderr
			// = our stderr (CH WARN), stdout = the kernel-dmesg sink per
			// --console. The returned consoleArg goes into the cmdline.
			cmd := exec.Command(opts.CHBinary)
			consoleArg, cleanup, err := opts.StdioMode.SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, fmt.Errorf("stdio: %w", err)
			}
			args, err := CHCommand(opts.Cfg, e.Blk0Sock, e.Blk1Sock, e.CHSock, e.VsockBase, kernelPath, runtimePath, e.UffdSock, consoleArg, e.TapFDNum, e.NetMAC)
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("CH cmdline: %w", err)
			}
			cmd.Args = append(cmd.Args, args...)
			logf("CH args: %s", strings.Join(args, " "))
			return cmd, cleanup, nil
		},

		// Cold-start settle: ping ticker starts as soon as the launch
		// handshake completes (HelloDone = LaunchSpec sent); Settled +
		// balloon reconcile + controller heartbeat/sensor gate on the
		// guest's launch_ack (post-boot transient over). Fire-and-forget
		// so ServeAndWait proceeds to cmd.Wait.
		PostSpawn: func(pc PostSpawnCtx) error {
			go func() {
				select {
				case <-pc.Launch.HelloDone():
					pc.Pinger.Start(pc.Ctx)
				case <-pc.Ctx.Done():
					return
				}
				select {
				case <-pc.Launch.LaunchAckDone():
					if err := pc.Hooks.Settled(); err != nil {
						pc.Logf("settled: %v", err)
					}
					if pc.Balloon != nil {
						if err := pc.Balloon.Start(pc.Ctx); err != nil {
							pc.Logf("balloon: start: %v", err)
						}
					}
					if pc.Hooks.Enabled() {
						pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
						pc.Hooks.StartSensor(pc.Ctx, 64<<20)
					}
				case <-pc.Ctx.Done():
				}
			}()
			return nil
		},
	})
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
		"faults_loaded",
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
	ManifestCfg *ManifestConfig // required for --upload; the Ingester is built lazily per snapshot
	SandboxID   string          // required: snapshot.Take rejects empty
	Memfd       *memory.Memfd
	DiffPath    string
	Srv0        *vhost.Server
	Srv1        *vhost.Server
	CHSock      string
	RunDir      string
	Pinger      *Pinger      // optional; if non-nil, paused around quiesce/Take
	Reattach    func() error // optional; re-establishes the stdio MUX after --resume
	Logf        func(string, ...any)
}

// Handle dispatches one ctl snapshot_request. Public for restore.Run.
func (h *SnapshotHandler) Handle(req ctl.Request) (ctl.Response, error) {
	opts := RunOptions{Cfg: h.Cfg, ManifestCfg: h.ManifestCfg, SandboxID: h.SandboxID}
	return handleSnapshotRequest(req, opts, h.Memfd, h.DiffPath, h.Srv0, h.Srv1, h.CHSock, h.RunDir, h.Pinger, h.Reattach, h.Logf)
}

// handleSnapshotRequest executes one snapshot_request received via
// ctl.sock. Wraps pkg/sandbox/snapshot.Take with the runtime state
// owned by Run. Upload mode opens a fresh manifest.IngesterCloser
// here (per request) so the snapshot write path never shares a
// connection pool with the long-lived disk read path.
func handleSnapshotRequest(
	req ctl.Request,
	opts RunOptions,
	mfd *memory.Memfd,
	diffPath string,
	srv0, srv1 *vhost.Server,
	chSock, runDir string,
	pinger *Pinger,
	reattachMUX func() error, // re-establishes the stdio MUX after --resume; may be nil
	logf func(string, ...any),
) (resp ctl.Response, err error) {
	// Quiesce sequence (docs/sandbox.md §6.2 T2a, sandbox-runtime.md §3.4):
	// pause the ping ticker, then `quiesce` → the guest does sync +
	// drop_caches, stops reading the app's stdout/stderr (pty), runs the
	// graceful MUX_CLOSE handshake on the stdio MUX (the host's mux.Session
	// auto-replies MUX_CLOSE_ACK), then replies `quiesced`. quiesced ⇒
	// the MUX is closed + the app is blocked + the guest is in a clean
	// state — safe to /vm.pause via snapshot.Take. Quiesce failure aborts
	// this snapshot rather than capturing a half-open MUX; sandbox keeps
	// running.
	//
	// On the --resume path, snapshot.Take resumes CH but the MUX stays
	// closed; reattachMUX (below, after Take) `attach`es a fresh one so the
	// running app's stdio flows again. On the destroy path (resume_after=
	// false) we instead tear the VMM down once this response has flushed —
	// that is what makes `sandbox-ctl run` return (docs/sandbox.md §6.2 T8).
	defer func() {
		if err == nil && !req.ResumeAfter {
			go destroyAfterSnapshot(chSock, logf)
		}
	}()
	if pinger != nil {
		pinger.Pause()
		// Resume on the way out UNLESS this is the success + destroy
		// path: there the VM stays paused while destroyAfterSnapshot
		// tears it down, so a resumed pinger only spews misleading
		// `ping ... i/o timeout` lines against a paused/dying VM. On
		// error (sandbox keeps running) or --resume (sandbox resumed),
		// the pinger must come back.
		defer func() {
			if err == nil && !req.ResumeAfter {
				return
			}
			pinger.Resume()
		}()
		client := pinger.Client
		if client == nil {
			client = &HostClient{BasePath: filepath.Join(runDir, "vsock.sock"), Logf: logf}
		}
		if err := SendQuiesce(client); err != nil {
			logf("quiesce: %v (aborting snapshot)", err)
			return ctl.Response{}, fmt.Errorf("quiesce: %w", err)
		}
		logf("quiesce: guest acked (MUX closed), proceeding to /vm.pause")
	}

	if req.Upload && (opts.ManifestCfg == nil || opts.ManifestCfg.Store.Endpoint == "") {
		return ctl.Response{}, fmt.Errorf("upload mode requires --manifest-config or MANIFEST_CONFIG (manifest store endpoints)")
	}
	// --output 与 --upload 互斥(docs/sandbox.md §13.3)。CLI 已经做过这道
	// 校验,但 ctl.sock 协议是开放的,run 进程也守在最后一关。
	if req.Upload && req.OutDir != "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive")
	}
	if !req.Upload && req.OutDir == "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive; one is required")
	}
	stagingDir := filepath.Join(runDir, "snap-stage")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return ctl.Response{}, err
	}
	defer os.RemoveAll(stagingDir)

	// Build snapshot.cfg builder closure. Take() invokes it with the final
	// overlay_ref the sink produced (file://<sha256>.overlay in --output mode,
	// manifest://<key> in --upload mode) so snapshot.cfg's overlay.base is
	// final on first write — no post-hoc ZIP rewrite.
	cfg := opts.Cfg
	snapCfgBuilder := func(overlayRef string) ([]byte, error) {
		return buildSnapshotCfg(cfg, overlayRef)
	}

	// opts.SandboxID is always populated by the run path (generated when the
	// CLI didn't pass one); see Run() in this file.
	sandboxID := opts.SandboxID

	// Pick the sink. --output writes sparse, content-addressed local files;
	// --upload streams to a fresh Ingester (its own store client, never shared
	// with the long-lived read-side fetcher). Either way Take streams the
	// multi-GiB memory + overlay straight to the sink — only CH's tiny
	// config.json/state.json transit the /run tmpfs staging dir.
	var sink snapshot.SnapshotSink
	var ingestSink *snapshot.IngestSink
	if req.Upload {
		ing, ierr := opts.ManifestCfg.NewIngester(opts.ManifestCfg.IngestKeyFunc(), nil)
		if ierr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot ingester: %w", ierr)
		}
		defer ing.Close()
		ingestSink = snapshot.NewIngestSink(ing, logf)
		sink = ingestSink
	} else {
		sink = snapshot.NewFileSink(req.OutDir, sandboxID, logf)
	}

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
	out, err := snapshot.Take(src, sink, req.ResumeAfter)
	if err != nil {
		return ctl.Response{}, err
	}

	// --resume: Take has resumed CH, but the guest closed the stdio MUX
	// during quiesce. Re-establish it (best-effort — the snapshot itself
	// succeeded; a reattach failure just leaves the resumed sandbox with
	// detached stdio). Do this before any upload work so the app, which is
	// already running again, isn't blocked on a full stdout pipe for long.
	if req.ResumeAfter && reattachMUX != nil {
		if err := reattachMUX(); err != nil {
			logf("stdio MUX re-attach after snapshot failed: %v (sandbox running, stdio detached)", err)
		} else {
			logf("stdio MUX re-attached after snapshot")
		}
	}

	resp = ctl.Response{
		MemorySize:       out.MemorySize,
		MemoryResident:   out.MemoryResident,
		WallclockPauseMs: out.WallclockPauseMs,
		WallclockDumpMs:  out.WallclockDumpMs,
	}
	if !req.Upload {
		resp.SnapshotPath = out.SnapshotPath
		resp.OverlayPath = out.OverlayPath
		resp.OverlayRef = out.OverlayRef
		return resp, nil
	}

	// Upload mode: the IngestSink already streamed the overlay + bundle to the
	// store during Take (resident pages only). Report the manifest keys it
	// produced (out.*Ref are manifest://<key>) plus per-artifact dedup stats.
	// OverlayRef stays scheme-tagged — it is the snapshot bundle's overlay.base,
	// used for from_refs chaining.
	overlayRes, bundleRes := ingestSink.Results()
	resp.SnapshotManifestKey = strings.TrimPrefix(out.SnapshotRef, "manifest://")
	resp.OverlayManifestKey = strings.TrimPrefix(out.OverlayRef, "manifest://")
	resp.OverlayRef = out.OverlayRef
	resp.Msg = fmt.Sprintf("upload OK; overlay stored=%d dedup=%d, snapshot stored=%d dedup=%d",
		overlayRes.StoredChunks, overlayRes.DedupChunks, bundleRes.StoredChunks, bundleRes.DedupChunks)
	return resp, nil
}

// destroyAfterSnapshotDelay is how long destroyAfterSnapshot waits before
// tearing the VMM down — long enough for the snapshot_done response to
// flush over ctl.sock back to the `sandbox-ctl snapshot` CLI (a tiny JSON
// over a local UDS; this margin is generous).
const destroyAfterSnapshotDelay = 300 * time.Millisecond

// destroyAfterSnapshot tears the VMM down (PUT /api/v1/vmm.shutdown) so
// the `sandbox-ctl run` process owning this ctl.sock returns. Run on a
// goroutine on the resume_after=false ("destroy") path: by the time the
// delay elapses the snapshot_done response has been queued + sent. Errors
// are only logged — the sandbox is being torn down regardless.
func destroyAfterSnapshot(chSock string, logf func(string, ...any)) {
	time.Sleep(destroyAfterSnapshotDelay)
	if err := snapshot.CHShutdownVMM(chSock); err != nil {
		logf("snapshot: destroy mode — vmm.shutdown: %v", err)
		return
	}
	logf("snapshot: destroy mode — VMM shutdown requested")
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
	// Incremental layered chain (docs/sandbox.md §3.5): prepend the parent
	// this run was restored from. Cold start ⇒ empty provenance ⇒ empty chains.
	doc.FromRefs = prependRef(cfg.SnapshotProvenance.ParentSnapshotRef, cfg.SnapshotProvenance.ParentFromRefs)
	doc.Boot.Root.Overlay.BaseFromRefs = prependRef(cfg.SnapshotProvenance.ParentOverlayBase, cfg.SnapshotProvenance.ParentBaseFromRefs)
	return yaml.Marshal(&doc)
}

// prependRef returns [ref] ++ rest when ref is non-empty, else nil. Used to
// extend a layered chain by the parent's top layer.
func prependRef(ref string, rest []string) []string {
	if ref == "" {
		return nil
	}
	out := make([]string, 0, 1+len(rest))
	out = append(out, ref)
	out = append(out, rest...)
	return out
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
	FromRefs []string `yaml:"from_refs,omitempty"`
	Boot     struct {
		RuntimeRef string `yaml:"runtime_ref"`
		Root       struct {
			BaseRef string `yaml:"base_ref"`
			Overlay struct {
				Base         string   `yaml:"base"`
				BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
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
