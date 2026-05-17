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
	"sync"
	"syscall"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/memory"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/ctl"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/mux"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/stdio"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
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
	StartUnixNs        int64  // T0 for the stats wallclock window
	StatsJSONPath      string // if non-empty, dump the stats JSON on exit

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
	Balloon       *BalloonController
	Hooks         *ControllerHooks

	SnapCfg     *SandboxConfig // ctl.sock SnapshotHandler.Cfg
	ManifestCfg *ManifestConfig
	DiffPath    string

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
		Path:    uffdSockPath,
		AddrMap: addrMap,
		Logf:    logf,
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
		Path:         launchSock,
		Spec:         p.LaunchSpec,
		Logf:         logf,
		OnAppStarted: func(pid int) { logf("guest reports user app pid=%d", pid) },
		OnAppExited:  func(code int) { logf("guest reports user app exited code=%d", code) },
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
	// (cold: after launch handshake; restore: after restore_ack).
	pinger := &Pinger{
		Client: &HostClient{BasePath: vsockBase, Logf: logf},
		Cfg:    PingerConfig{FatalThreshold: p.PingFatalThreshold},
		Stats:  &PingStats{},
		Logf:   logf,
	}

	// ctl.sock server for snapshot requests. SnapshotHandler is the
	// shared bundle both Run and restore.Run use.
	snapHandler := &SnapshotHandler{
		Cfg:         p.SnapCfg,
		ManifestCfg: p.ManifestCfg,
		SandboxID:   p.SandboxID,
		Memfd:       memfd,
		DiffPath:    p.DiffPath,
		Srv0:        srv0,
		Srv1:        srv1,
		CHSock:      chSock,
		RunDir:      runDir,
		Pinger:      pinger,
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

	defer pinger.Stop()
	defer func() {
		if p.Balloon != nil {
			p.Balloon.Stop()
		}
	}()

	cmd, chStdioCleanup, err := p.BuildCmd(CmdEnv{
		Memfd:     memfd,
		CHSock:    chSock,
		Blk0Sock:  blk0Sock,
		Blk1Sock:  blk1Sock,
		VsockBase: vsockBase,
		UffdSock:  uffdSockPath,
		RunDir:    runDir,
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
