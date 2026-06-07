package sandbox

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/ctl"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/memory"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/mux"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/proto"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/stdio"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/uffd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/vhost"
	"golang.org/x/sys/unix"
)

// CmdEnv carries the resolved per-sandbox socket paths + the memfd to a
// caller's BuildCmd closure. ServeAndWait owns both the path layout and
// the memfd, so the cold / restore CH cmdlines (which differ — full
// boot args vs `--restore source_url=`) are the only thing the callers
// supply, computed against paths ServeAndWait already decided.
type CmdEnv struct {
	Memfd     *memory.Memfd
	CHSock    string
	Blk0Sock  string
	Blk1Sock  string
	VsockBase string
	UffdSock  string
	RunDir    string

	// TapFDNum is the CH-visible fd of the inherited tap queue (cmd.ExtraFiles
	// after memfd → 4), or 0 when there is no fd-handoff network (tap-name or
	// no network). NetMAC is the effective virtio-net MAC the cmd builder puts
	// in CH's --net mac= (tapfd metadata override or config). See docs/sandbox.md.
	TapFDNum int
	NetMAC   string
}

// PostSpawnCtx is handed to a caller's PostSpawn closure right after CH
// is started. It exposes exactly the shared machinery the two settle
// protocols need:
//
//   - cold start: spawn a goroutine that gates pinger.Start on
//     Launch.HelloDone and Hooks.Settled + Balloon.Start on
//     Launch.LaunchAckDone, then return nil (fire-and-forget).
//   - restore: synchronously waitAPI → /vm.resume → OpenMUXViaRestore →
//     EstablishMUX → Pinger.Start → Balloon.Start → Hooks.SettledRestore;
//     a non-nil return aborts the run (ServeAndWait kills CH).
type PostSpawnCtx struct {
	Ctx          context.Context
	Cmd          *exec.Cmd
	Pinger       *Pinger
	Launch       *LaunchServer
	EstablishMUX func(net.Conn, proto.StdioSpec) error
	Hooks        *ControllerHooks
	Balloon      *BalloonController
	CHSock       string
	Logf         func(string, ...any)
}

// VMParams is the input to ServeAndWait — everything the shared
// back-half needs that differs between cold start and restore. The
// callers (sandbox.Run / restore.Run) keep their own thin front-half
// (config resolution, cgroup, controller admit) and converge here.
type VMParams struct {
	Ctx                context.Context
	SandboxID          string
	RunDir             string // already created by the caller (caller defers RemoveAll)
	Logf               func(string, ...any)
	StdioMode          stdio.Mode
	PingFatalThreshold int
	StartUnixNs        int64         // T0 for the stats wallclock window
	StatsJSONPath      string        // if non-empty, dump the stats JSON on exit
	StatsInterval      time.Duration // if > 0, periodically log lazy-load stats (uffd + vhost); 0 = off

	CapBytes   int64               // RAM capacity (memfd size); also stats UffdRAMSize
	UffdSource uffd.SnapshotReader // ZeroSource (cold) | snapshot source (restore)
	Blk0Reader vhost.BlockReader
	Blk0Label  string
	Blk0Path   string
	Cow        *vhost.BlockCOW
	Blk1Label  string
	Blk1Path   string

	LaunchSpec    *proto.LaunchSpec // cold: real spec; restore: &proto.LaunchSpec{} placeholder
	WireLaunchMUX bool              // cold: true (launch conn → MUX); restore: false (MUX via PostSpawn)
	StartTimeout  time.Duration     // host wait for launch_ack (covers guest init); 0 = indefinite
	// VAReportDeadline bounds the CH→host uffd-fd handoff handshake (shared
	// cold/restore); 0 = no forced timeout. From cfg.VAReportDeadline().
	VAReportDeadline time.Duration
	// PingTimeout bounds the host→guest ping round-trip; 0 → no forced timeout
	// (resolved to NoForcedTimeout, since DialRaw needs a value). cfg.PingDeadline().
	PingTimeout time.Duration
	// AppNotifyDeadline bounds the host read of one guest→host launch-port
	// message (hello / app_started / app_exited / mem_report); 0 = no forced
	// timeout. From cfg.AppNotifyDeadline().
	AppNotifyDeadline time.Duration
	Balloon           *BalloonController
	Hooks             *ControllerHooks

	// TapFile, when non-nil, is a tap queue fd acquired via the tapfd handoff
	// (docs/tapfd.md). ServeAndWait inherits it into CH after the memfd (CH
	// fd 4) and surfaces CmdEnv.TapFDNum=4; the caller closes it after the run.
	// NetMAC is the effective virtio-net MAC, surfaced as CmdEnv.NetMAC.
	TapFile *os.File
	NetMAC  string

	// Cgroup, when active, gets the CH PID appended after cmd.Start so CH is
	// the only process in the per-sandbox cgroup. sandbox-ctl deliberately
	// stays in its parent cgroup — see pkg/sandbox/cgroup.go header for the
	// memcg-throttle deadlock that caused.
	Cgroup *CgroupController

	// NetnsFile, when non-nil, is the tap's network-namespace fd from the same
	// handoff (docs/tapfd.md §4.6). ServeAndWait fork/execs CH on a thread that
	// setns()'d into it, so CH runs inside the tap's netns. CH does NOT inherit
	// this fd (it's not an ExtraFile); the caller closes it after the run.
	NetnsFile *os.File

	SnapCfg     *SandboxConfig // ctl.sock SnapshotHandler.Cfg
	ManifestCfg *ManifestConfig
	DiffPath    string
	OwnedDiff   bool // diff is auto-created (ours) → eligible for zero-copy move on destroy-snapshot

	// Forwards are the parsed `--connect` port-forward directives. Each
	// gets a host-local listener whose accepted connections are spliced to
	// a guest-side target via a reverse channel (docs/sandbox-runtime.md
	// §3.7). Empty → no port forwarding.
	Forwards []ForwardSpec

	BuildCmd  func(CmdEnv) (cmd *exec.Cmd, cleanup func(), err error)
	PostSpawn func(PostSpawnCtx) error
}

// ServeAndWait owns the half of the sandbox lifecycle that is identical
// between cold start and restore: memfd + uffd va_report handler,
// vhost-blk backends, the launch server (guest→host mem_report /
// app_exited — and, cold only, the launch→MUX upgrade), the pinger,
// the ctl.sock snapshot server, the backend goroutine fan-out, signal
// escalation around CH, and the stats dump. The genuinely divergent
// parts — config resolution, the CH cmdline, the post-spawn settle
// protocol — stay in the callers via VMParams.BuildCmd / PostSpawn.
func ServeAndWait(p VMParams) (int, error) {
	logf := p.Logf
	runDir := p.RunDir
	chSock := filepath.Join(runDir, "ch.sock")
	blk0Sock := filepath.Join(runDir, "blk0.sock")
	blk1Sock := filepath.Join(runDir, "blk1.sock")
	vsockBase := filepath.Join(runDir, "vsock.sock")
	launchSock := fmt.Sprintf("%s_%d", vsockBase, proto.LaunchPort)
	uffdSockPath := filepath.Join(runDir, "uffd.sock")
	ctlSockPath := filepath.Join(runDir, "ctl.sock")

	// backendCtx is the context the vhost / launch / va_report / ctl.sock
	// servers run under (and the stdio MUX bridge). Cancelled when CH
	// exits (or earlier via signal escalation); the deferred cancel is a
	// backstop for the early-error returns below.
	backendCtx, cancelBackends := context.WithCancel(p.Ctx)
	defer cancelBackends()

	// Exactly one stdio MUX at a time; which conn backs it changes across
	// launch → restore → attach. muxLink guards the pair so the snapshot
	// handler can re-attach (snapshot --resume) without racing teardown.
	var muxLink MUXLink
	reattach := func() error {
		return muxLink.Reattach(backendCtx, &HostClient{BasePath: vsockBase, Logf: logf}, p.StdioMode)
	}
	establishMUX := func(conn net.Conn, spec proto.StdioSpec) error {
		ss := stdio.StreamSetFor(spec)
		sess := mux.NewSession(conn, ss, mux.Options{})
		cleanup, err := p.StdioMode.Bridge(backendCtx, sess, ss)
		if err != nil {
			_ = sess.Close()
			return err
		}
		muxLink.Set(sess, cleanup)
		return nil
	}

	// Memory setup. sandbox-ctl owns the memfd; CH inherits it via
	// cmd.ExtraFiles[0]=memfd. The uffd is created in CH's process and
	// handed back via SCM_RIGHTS in the va_report message.
	memfd, err := memory.Create("sandbox-"+p.SandboxID+"-ram", p.CapBytes)
	if err != nil {
		return -1, fmt.Errorf("memfd create: %w", err)
	}
	defer memfd.Close()

	// AddressMap is shared between the va_report server (registers
	// ProcessCH on receive) and the uffd Handler (Locate on each fault).
	addrMap := uffd.NewAddressMap(uint64(memfd.Size()))
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
		Path:              uffdSockPath,
		AddrMap:           addrMap,
		Logf:              logf,
		HandshakeDeadline: p.VAReportDeadline,
		OnReady: func(uffdFD int, vaStart, size uint64) error {
			h, err := uffd.NewWithBackendUffd(uffdFD, addrMap, uffd.Config{
				MemfdFD:   memfd.FD(),
				BackendVA: memfd.Addr(),
				Size:      memfd.Size(),
				Source:    p.UffdSource,
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

	// vhost-blk backends.
	srv0 := vhost.NewServer(blk0Sock, &vhost.ReadOnlyBackend{R: p.Blk0Reader}, logf)
	srv0.EnableStats(p.Blk0Label, p.Blk0Path)
	srv0.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv0.Listen(); err != nil {
		return -1, err
	}
	srv1 := vhost.NewServer(blk1Sock, &vhost.CowBackend{C: p.Cow}, logf)
	srv1.EnableStats(p.Blk1Label, p.Blk1Path)
	srv1.SetMemfd(memfd.Inode(), memfd.Bytes())
	if err := srv1.Listen(); err != nil {
		srv0.Stop()
		return -1, err
	}

	// LaunchServer: guest→host management short-conns on
	// <vsock-base>_5000. Both cold and restore need this for the
	// periodic mem_report (host-side BalloonController) and app_exited.
	// Cold additionally upgrades the hello/launch_ack conn to the stdio
	// MUX (OnMUXReady); restore's MUX comes from the reverse channel
	// (PostSpawn → OpenMUXViaRestore), so OnMUXReady stays nil there and
	// the placeholder Spec is never sent (guest doesn't re-hello after a
	// restore).
	launch := &LaunchServer{
		Path:              launchSock,
		Spec:              p.LaunchSpec,
		StartTimeout:      p.StartTimeout,
		AppNotifyDeadline: p.AppNotifyDeadline,
		Logf:              logf,
		OnAppStarted:      func(pid int) { logf("guest reports user app pid=%d", pid) },
		OnAppExited:       func(code int) { logf("guest reports user app exited code=%d", code) },
		OnMemReport: func(memAvail, memTotal uint64) {
			if p.Balloon != nil && os.Getenv("SANDBOX_BALLOON_NO_HINT") == "" {
				p.Balloon.Hint(memAvail, memTotal)
			}
		},
	}
	if p.WireLaunchMUX {
		launch.OnMUXReady = func(conn net.Conn, established proto.StdioSpec) {
			if err := establishMUX(conn, established); err != nil {
				logf("stdio MUX bridge: %v", err)
				return
			}
			logf("stdio MUX established (tty=%v stdin=%v stdout=%v stderr=%v)",
				established.TTY, established.Stdin, established.Stdout, established.Stderr)
		}
	}
	if err := launch.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, err
	}

	// Pinger drives the host→guest health probe. Started by PostSpawn
	// (cold: after launch handshake; restore: after restore_ack). 0 ping
	// timeout → no forced timeout (resolved to NoForcedTimeout, since the
	// ping RoundTrip dials and DialRaw needs a finite value).
	pingTO := p.PingTimeout
	if pingTO <= 0 {
		pingTO = NoForcedTimeout
	}
	pinger := &Pinger{
		Client: &HostClient{BasePath: vsockBase, Logf: logf},
		Cfg:    PingerConfig{FatalThreshold: p.PingFatalThreshold, Timeout: pingTO},
		Stats:  &PingStats{},
		Logf:   logf,
	}

	// Port-forward listeners. Built here so the snapshot handler can pause
	// + collapse active relays around quiesce (symmetric with the pinger);
	// started below alongside the other backend servers.
	forwarder := NewForwarder(vsockBase, logf)

	// ctl.sock server for snapshot requests. SnapshotHandler is the
	// shared bundle both Run and restore.Run use.
	snapHandler := &SnapshotHandler{
		Cfg:         p.SnapCfg,
		ManifestCfg: p.ManifestCfg,
		SandboxID:   p.SandboxID,
		Memfd:       memfd,
		DiffPath:    p.DiffPath,
		OwnedDiff:   p.OwnedDiff,
		Srv0:        srv0,
		Srv1:        srv1,
		CHSock:      chSock,
		RunDir:      runDir,
		Pinger:      pinger,
		Forwarder:   forwarder,
		Reattach:    reattach,
		Logf:        logf,
	}
	ctlSrv := &ctl.Server{
		Path:            ctlSockPath,
		Logf:            logf,
		SnapshotHandler: snapHandler.Handle,
		ExecHandler: func(conn net.Conn, req ctl.Request) {
			serveExecRequest(backendCtx, conn, req, vsockBase, logf)
		},
	}
	if err := ctlSrv.Listen(); err != nil {
		srv0.Stop()
		srv1.Stop()
		return -1, fmt.Errorf("ctl.sock listen: %w", err)
	}
	defer ctlSrv.Stop()
	// By function return the servers are already stopped (explicit
	// cancelBackends()+backendWG.Wait() below), so this just closes the
	// MUX conn and runs the bridge cleanup (restores the terminal in tty
	// mode, drains the guest→host pumps).
	defer muxLink.Teardown()

	var backendWG sync.WaitGroup
	backendWG.Add(5)
	go func() { defer backendWG.Done(); _ = srv0.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = srv1.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = launch.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = vaReportSrv.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = ctlSrv.Serve(backendCtx) }()

	// Optional periodic lazy-load stats logger (observe a slow remote/cache or
	// backed-up fault queue in real time; quiet once warm). Ends on backendCtx.
	if p.StatsInterval > 0 {
		backendWG.Add(1)
		go func() {
			defer backendWG.Done()
			lazyStatsTicker(backendCtx, p.StatsInterval, func() *uffd.Handler {
				uffdHandlerMu.Lock()
				defer uffdHandlerMu.Unlock()
				return uffdHandler
			}, srv0, srv1, logf)
		}()
	}

	// Port-forward accept loops run under backendCtx (they end when CH
	// exits / the run unwinds). A bad listener (e.g. fd= not a socket)
	// aborts the run. Close on the way out tears down listeners + relays.
	if err := forwarder.Start(backendCtx, p.Forwards); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("port-forward: %w", err)
	}
	defer forwarder.Close()

	defer pinger.Stop()
	defer func() {
		if p.Balloon != nil {
			p.Balloon.Stop()
		}
	}()

	// memfd is cmd.ExtraFiles[0] → CH fd 3; an optional tapfd-handoff queue
	// fd is appended next → CH fd 4 (referenced by --net fd= / restore net_fds).
	tapFDNum := 0
	if p.TapFile != nil {
		tapFDNum = 4
	}
	cmd, chStdioCleanup, err := p.BuildCmd(CmdEnv{
		Memfd:     memfd,
		CHSock:    chSock,
		Blk0Sock:  blk0Sock,
		Blk1Sock:  blk1Sock,
		VsockBase: vsockBase,
		UffdSock:  uffdSockPath,
		RunDir:    runDir,
		TapFDNum:  tapFDNum,
		NetMAC:    p.NetMAC,
	})
	if err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("build CH cmd: %w", err)
	}
	defer chStdioCleanup()
	// fd=3 ← memfd in CH (after stdin/out/err). New process group keeps
	// CH out of sandbox-ctl's controlling-terminal foreground group, so
	// terminal-generated ^C/^\/^Z don't hit CH directly — sandbox-ctl
	// owns signal handling (below).
	cmd.ExtraFiles = []*os.File{memfd.File()}
	if p.TapFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, p.TapFile) // CH fd 4
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	if err := startCH(cmd, p.NetnsFile); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("spawn CH: %w", err)
	}
	chPid := cmd.Process.Pid
	logf("CH started pid=%d", chPid)

	// Move CH (and only CH) into the per-sandbox cgroup. sandbox-ctl
	// stays in its parent cgroup — see pkg/sandbox/cgroup.go header.
	// Failure is fatal: missing cgroup enforcement on CH is worse than
	// taking the boot down so the orchestrator restarts cleanly.
	if err := p.Cgroup.AddPID(chPid); err != nil {
		_ = cmd.Process.Kill()
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("cgroup: move CH (pid=%d) in: %w", chPid, err)
	}

	// PingFatalThreshold wiring: when the threshold is hit (opt-in),
	// SIGTERM CH so cmd.Wait returns; waitForCHWithSignalEscalation then
	// escalates to SIGKILL after chShutdownGrace if CH doesn't drain.
	pinger.SetOnFatal(func(err error) {
		logf("pinger fatal threshold (%d) hit: %v — SIGTERMing CH pid=%d",
			pinger.Cfg.FatalThreshold, err, chPid)
		_ = cmd.Process.Signal(syscall.SIGTERM)
	})

	if err := p.PostSpawn(PostSpawnCtx{
		Ctx:          backendCtx,
		Cmd:          cmd,
		Pinger:       pinger,
		Launch:       launch,
		EstablishMUX: establishMUX,
		Hooks:        p.Hooks,
		Balloon:      p.Balloon,
		CHSock:       chSock,
		Logf:         logf,
	}); err != nil {
		_ = cmd.Process.Kill()
		cancelBackends()
		backendWG.Wait()
		return -1, err
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	waitErr := waitForCHWithSignalEscalation(doneCh, sigCh, cmd.Process, chPid, chSock, chShutdownGrace, logf)
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

	// Route stats through log.Default().Writer(): when stdio.Bridge ran
	// in tty mode it CRLF-wrapped that writer (paired with makeRaw), so
	// these multi-line blocks don't stairstep on a still-raw terminal.
	statsW := log.Default().Writer()
	_, _ = srv0.WriteStatsTo(statsW)
	_, _ = srv1.WriteStatsTo(statsW)
	uffdHandlerMu.Lock()
	h := uffdHandler
	uffdHandlerMu.Unlock()
	if h != nil {
		writeUffdStats(statsW, h.Stats())
	}
	if p.StatsJSONPath != "" {
		bundle := statsBundle{
			Servers:     []*vhost.Server{srv0, srv1},
			StartUnixNs: p.StartUnixNs,
			EndUnixNs:   time.Now().UnixNano(),
		}
		if h != nil {
			bundle.Uffd = h.Stats()
			bundle.UffdRAMSize = p.CapBytes
		}
		if pinger.Stats != nil {
			snap := pinger.Stats.Snapshot()
			bundle.Ping = &snap
		}
		if err := writeStatsJSON(p.StatsJSONPath, bundle); err != nil {
			logf("stats json write %s: %v", p.StatsJSONPath, err)
		} else {
			logf("stats json written to %s", p.StatsJSONPath)
		}
	}
	return exit, nil
}

// startCH starts cmd. When netnsFile is non-nil the fork/exec runs on a thread
// moved into that network namespace (docs/tapfd.md §4.6), so CH — and thus the
// guest's virtio-net — lives inside the tap's netns; otherwise CH starts in the
// host netns. cmd.Process is populated by the time this returns.
func startCH(cmd *exec.Cmd, netnsFile *os.File) error {
	if netnsFile == nil {
		return cmd.Start()
	}
	errCh := make(chan error, 1)
	go func() {
		// setns(CLONE_NEWNET) changes only the calling thread's netns, so lock
		// the goroutine to its OS thread: the scheduler must not migrate us and
		// the change must not leak to other goroutines. We deliberately never
		// UnlockOSThread — this thread now sits in the tap's netns, so let the
		// runtime retire it when the goroutine returns rather than reuse it for
		// host-netns work.
		runtime.LockOSThread()
		if err := unix.Setns(int(netnsFile.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("setns(CLONE_NEWNET): %w", err)
			return
		}
		// fork/exec inherits this thread's netns → CH lands in the tap's netns.
		errCh <- cmd.Start()
	}()
	return <-errCh
}
