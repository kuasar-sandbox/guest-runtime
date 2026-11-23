package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// executionControl is the boundary between Guest management and user-process
// execution. Production management uses executorClient; supervisorState owns
// these operations inside the inner init. No user process runs in the outer.
type executionControl interface {
	stdioSpec() proto.StdioSpec
	runSession(*vsockConn, *proto.Message) (bool, error)
	quiesce() error
	prepareReattach() error
	attachSession(*vsockConn) (bool, error)
}

type executorClient struct {
	link        *executorLink
	process     *os.Process
	spec        *proto.LaunchSpec
	ready       atomic.Bool
	stopping    atomic.Bool
	terminal    atomic.Bool
	ended       chan struct{}
	endOnce     sync.Once
	report      sync.Once
	admission   sync.RWMutex // no requests can be sent to a frozen inner init
	paused      bool
	systemd     *systemdEnvironment
	processDone chan struct{} // real environment PID 1 Wait, independent of the executor link
	processErr  error         // published by closing processDone
	stopMu      sync.Mutex    // stop latch versus lifecycle response commit; never held across I/O
	stopFact    environmentStopFact
}

// innerEvents exists only in the inner re-exec. Helpers exec a fresh image and
// do not inherit it. Lifecycle facts and VM poweroff remain owned by the outer.
var innerEvents *executorLink

func startExecutor(spec *proto.LaunchSpec) (*executorClient, *exec.Cmd, error) {
	// Mounts added by the outer later (file injection and disk slots) flow into
	// the inner's slave mount tree, and through its shared peers to user mounts.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return nil, nil, fmt.Errorf("make outer mounts shared: %w", err)
	}
	installation, err := selectSystemd(spec)
	if err != nil {
		return nil, nil, err
	}
	if installation != nil {
		return startSystemdExecutor(spec, installation)
	}
	link, childLink, err := newExecutorPair()
	if err != nil {
		return nil, nil, err
	}
	parentSync, childSync, err := newChildSyncPair()
	if err != nil {
		link.close()
		_ = childLink.Close()
		return nil, nil, err
	}
	attr := &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS}
	if err := cgroupConfigureClone(attr, true); err != nil {
		link.close()
		_ = childLink.Close()
		_ = parentSync.Close()
		_ = childSync.Close()
		return nil, nil, err
	}
	control, groups := "0", "0"
	if spec.CgroupControl {
		control = "1"
	}
	if len(guestSlots.disks) != 0 {
		groups = "1"
	}
	cmd := &exec.Cmd{
		Path: "/proc/self/exe", Args: []string{"sandbox-init", "inner", control, groups},
		Env: os.Environ(), Dir: "/", Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: attr, ExtraFiles: []*os.File{childLink, childSync},
	}
	client := &executorClient{link: link, spec: spec, ended: make(chan struct{})}
	go link.serve(client.handleEvent)
	_, err = coordinateChild(cmd, parentSync, childSync, func(int) error {
		client.process = cmd.Process
		return nil
	}, cgroupFinalizeBootstrap, nil, childSyncStarted)
	_ = childLink.Close()
	if err != nil {
		link.close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil, nil, fmt.Errorf("inner bootstrap: %w", err)
	}
	client.startProcessWait(cmd)
	return client, cmd, nil
}

func (c *executorClient) startProcessWait(cmd *exec.Cmd) {
	c.processDone = make(chan struct{})
	go func() {
		c.processErr = cmd.Wait()
		close(c.processDone)
	}()
}

func (c *executorClient) launch(conn *vsockConn) error {
	c.admission.RLock()
	defer c.admission.RUnlock()
	// runInit may legitimately take longer than an ordinary management RPC.
	// The Host's existing launch timeout and process lifetime own this budget.
	_, err := c.transfer(context.Background(), executorMessage{Op: "launch", Launch: c.spec}, conn)
	return err
}

func (c *executorClient) handleEvent(msg executorMessage, fd *int) (executorMessage, error) {
	switch msg.Op {
	case "systemd-failed":
		if c.systemd == nil {
			return executorMessage{}, errors.New("unexpected systemd setup failure")
		}
		select {
		case c.systemd.attached <- fmt.Errorf("systemd executor setup: %s", msg.Error):
		default:
		}
		return executorMessage{}, nil
	case "systemd-attach":
		if c.systemd == nil {
			return executorMessage{}, errors.New("unexpected systemd attach")
		}
		return executorMessage{}, c.attachSystemd(msg, fd)
	case "systemd-ready":
		if c.systemd == nil {
			return executorMessage{}, errors.New("unexpected systemd READY")
		}
		c.systemd.mu.Lock()
		defer c.systemd.mu.Unlock()
		if c.systemd.peer == nil {
			return executorMessage{}, errors.New("systemd READY before verified attach")
		}
		if err := c.confirmSystemdReady(); err != nil {
			return executorMessage{}, err
		}
		select {
		case <-c.systemd.ready:
			return executorMessage{}, errors.New("duplicate systemd READY")
		default:
			close(c.systemd.ready)
		}
		return executorMessage{}, nil
	case "systemd-peer-fd":
		if c.systemd == nil {
			return executorMessage{}, errors.New("unexpected systemd peer")
		}
		c.systemd.receivePeer(msg, fd)
		return executorMessage{}, nil
	case "stopping":
		if c.systemd != nil {
			proof := stopUnverified
			if msg.Reason == "primary-terminal" {
				proof = stopNormal
			}
			c.beginEnvironmentStop("executor "+msg.Reason, proof)
		}
		return executorMessage{}, nil
	case "identity":
		if *fd < 0 {
			return executorMessage{}, errors.New("missing primary pidfd")
		}
		pid, err := outerPIDFromFD(*fd)
		return executorMessage{PID: pid}, err
	case "started":
		if msg.PID <= 0 {
			return executorMessage{}, errors.New("invalid primary PID")
		}
		initialLaunch.started(msg.PID)
		c.ready.Store(true)
		if err := notifyAppStarted(msg.PID); err != nil {
			logf("app_started notify failed (continuing): %v", err)
		}
		c.report.Do(func() { go runMemReporter(memReportInterval) })
		return executorMessage{}, nil
	case "exited":
		if c.systemd != nil {
			proof := stopUnverified
			if msg.Reason == "primary-terminal" {
				proof = stopNormal
			}
			if msg.Reason == "executor-failed" {
				proof = stopFailed
			}
			c.beginEnvironmentStop("executor "+msg.Reason, proof)
		}
		notifyAppExited(msg.Code, msg.Signal, msg.Reason)
		c.terminal.Store(true)
		c.endOnce.Do(func() { close(c.ended) })
		return executorMessage{}, nil
	default:
		return executorMessage{}, fmt.Errorf("unknown inner event %q", msg.Op)
	}
}

func (c *executorClient) stdioSpec() proto.StdioSpec {
	// Match consoleBridge.protoSpec: reconnect carries established streams,
	// while the Host resends the current terminal size through MUX control.
	s := c.spec.Stdio
	if s.TTY {
		return proto.StdioSpec{TTY: true}
	}
	return proto.StdioSpec{Stdin: s.Stdin, Stdout: s.Stdout, Stderr: s.Stderr}
}

// transfer has a single ownership rule: sending rights ends the outer's use of
// this connection, regardless of whether a reply subsequently arrives. Release
// closes only this descriptor, never shutdown(2), which would break the inner's
// duplicate. A failed exchange is not retried with the same descriptor.
func (c *executorClient) transfer(ctx context.Context, msg executorMessage, conn *vsockConn) (bool, error) {
	defer conn.release()
	_, err := c.link.call(ctx, msg, conn.fd)
	return true, err
}

func (c *executorClient) runSession(conn *vsockConn, req *proto.Message) (bool, error) {
	c.admission.RLock()
	defer c.admission.RUnlock()
	if c.paused || c.stopping.Load() {
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "sandbox is quiescing"})
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.transfer(ctx, executorMessage{Op: "session", Request: req}, conn)
}

func (c *executorClient) quiesce() error {
	c.admission.Lock()
	defer c.admission.Unlock()
	if c.stopping.Load() {
		return errors.New("cannot quiesce a stopping environment")
	}
	c.paused = true
	_, err := c.link.request(executorMessage{Op: "quiesce"}, -1)
	if err == nil && c.systemd != nil {
		err = c.systemd.observer.Load().request(true)
	}
	if err == nil && c.stopping.Load() {
		err = errors.New("environment stopped during quiesce")
	}
	return err
}

// A lifecycle response commits before its bounded socket write. Concurrent
// stop can retire that committed state, but an already accepted stop can never
// be published as capturable/runnable. Keeping I/O outside stopMu ensures even
// a blocked Host response cannot delay the stop latch and its absolute timer.
func (c *executorClient) commitLifecycleResponse() error {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	if c.stopping.Load() {
		return errors.New("environment is stopping")
	}
	return nil
}

func (c *executorClient) prepareReattach() error {
	c.admission.Lock()
	defer c.admission.Unlock()
	if c.systemd != nil {
		if err := c.systemd.observer.Load().request(false); err != nil {
			return err
		}
		if c.stopping.Load() {
			return errors.New("systemd environment is stopping")
		}
	}
	_, err := c.link.request(executorMessage{Op: "resume"}, -1)
	if err == nil {
		c.paused = false
	}
	return err
}

func (c *executorClient) attachSession(conn *vsockConn) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.transfer(ctx, executorMessage{Op: "attach"}, conn)
}

func (c *executorClient) shutdown() {
	c.beginEnvironmentStop("Host shutdown", stopNormal)
}

type innerExecutor struct {
	mu         sync.Mutex
	launchOnce bool
	supervisor *supervisorState
}

func runInner(control, groups bool) {
	sync, err := childSyncFromFD(4)
	if err != nil {
		die("inner handshake: %v", err)
	}
	if err := sync.waitFor(childMsgStart); err != nil {
		sync.fail(err)
	}
	if err := prepareInnerEnvironment(control, groups); err != nil {
		sync.fail(err)
	}
	if err := sync.readyAndWait(); err != nil {
		sync.fail(err)
	}
	// shared+slave: receive outer mount events and forward them to children,
	// while keeping inner and user mount operations out of the outer namespace.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		sync.fail(fmt.Errorf("make inner mounts shared: %w", err))
	}
	file := os.NewFile(3, "inner-control")
	unix.CloseOnExec(3)
	link, err := executorLinkFromFile(file)
	_ = file.Close()
	if err != nil {
		sync.fail(err)
	}
	innerEvents = link
	inner := &innerExecutor{}
	go link.serve(inner.handle)
	if err := sync.write(childMsgExec); err != nil {
		die("inner bootstrap complete: %v", err)
	}
	sync.close()
	<-link.done
	die("outer management connection closed")
}

func takeExecutorConn(fd *int) (*vsockConn, error) {
	if *fd < 0 {
		return nil, errors.New("missing execution connection")
	}
	conn := &vsockConn{fd: *fd}
	*fd = -1
	return conn, nil
}

func (e *innerExecutor) handle(msg executorMessage, fd *int) (executorMessage, error) {
	// This control-only refresh must not wait behind Launch/Init's mutex.
	// It neither admits execution nor consumes the one-shot Launch state.
	if msg.Op == "systemd-peer" {
		ctx, cancel := context.WithTimeout(context.Background(), systemdObserveQuery)
		defer cancel()
		peer, err := connectSystemdManager(ctx)
		if peer >= 0 {
			defer unix.Close(peer)
		}
		response := executorMessage{Op: "systemd-peer-fd", Epoch: msg.Epoch}
		if err != nil {
			response.Error = err.Error()
		}
		return executorMessage{}, innerEvents.sendControl(ctx, response, peer)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if msg.Op == "launch" {
		if e.launchOnce || msg.Launch == nil {
			return executorMessage{}, errors.New("invalid or duplicate inner launch")
		}
		e.launchOnce = true
		conn, err := takeExecutorConn(fd)
		if err != nil {
			return executorMessage{}, err
		}
		return executorMessage{}, e.launch(msg.Launch, conn)
	}
	sup := e.supervisor
	if sup == nil {
		return executorMessage{}, errors.New("workload not launched")
	}
	switch msg.Op {
	case "session", "attach":
		conn, err := takeExecutorConn(fd)
		if err != nil {
			return executorMessage{}, err
		}
		var handed bool
		if msg.Op == "session" {
			if msg.Request == nil {
				err = errors.New("missing session request")
			} else {
				handed, err = sup.runSession(conn, msg.Request)
			}
		} else {
			handed, err = sup.attachSession(conn)
		}
		if !handed {
			_ = conn.Close()
		}
		return executorMessage{}, err
	case "quiesce":
		return executorMessage{}, sup.quiesce()
	case "resume":
		return executorMessage{}, sup.prepareReattach()
	default:
		return executorMessage{}, fmt.Errorf("unknown execution operation %q", msg.Op)
	}
}

func (e *innerExecutor) launch(spec *proto.LaunchSpec, conn *vsockConn) error {
	if runtimeSystemdTools {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		if path, exists := spec.Env["PATH"]; !exists {
			spec.Env["PATH"] = executionDefaultPath()
			spec.SystemdPathDefault = true
		} else if spec.SystemdPathDefault {
			spec.Env["PATH"] = prependSystemdTools(path)
		}
	}
	cs, bridge, err := phase2Apply(spec, conn)
	if err != nil {
		return err
	}
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	sup := &supervisorState{
		bridge: bridge, spec: spec, stopSignal: syscall.Signal(spec.StopSignal),
		stopGrace: time.Duration(spec.StopGraceSec) * time.Second,
		execReg:   newExecRegistry(), connReg: newConnRegistry(),
		acceptLn: newAcceptListeners(), pluginReg: newPluginRegistry(),
	}
	pid, outerPID, err := phase2ForkApp(spec, cs, func(pid int) error {
		sup.appPid.Store(int64(pid))
		return nil
	})
	if err != nil {
		return fmt.Errorf("initial primary: %w", err)
	}
	logf("inner primary started pid=%d outer_pid=%d", pid, outerPID)
	sup.appBackoff.onStart(time.Now())
	sup.pluginReg.start(spec.Plugins)
	e.supervisor = sup
	if err := notifyAppStarted(outerPID); err != nil {
		return fmt.Errorf("publish primary start: %w", err)
	}
	go phase3Supervise(sup, bridge, sigCh)
	return nil
}

func (s *supervisorState) stdioSpec() proto.StdioSpec { return s.bridge.protoSpec() }

func (s *supervisorState) runSession(c *vsockConn, req *proto.Message) (bool, error) {
	if req.Type != proto.TypeExec && req.Type != proto.TypeConnect {
		return false, fmt.Errorf("invalid execution session %q", req.Type)
	}
	s.sessionMu.Lock()
	if s.sessionsPaused {
		s.sessionMu.Unlock()
		_ = c.SetLinger(muxCloseLingerSec)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "sandbox is quiescing"})
		return false, nil
	}
	// Receipt is registered synchronously, before the private handoff reply.
	// The full handler lifetime includes setup before the specialized exec or
	// forward registry and its final socket/target cleanup after those drains.
	s.sessions.Add(1)
	s.sessionMu.Unlock()
	_ = c.SetLinger(muxCloseLingerSec)
	go func() {
		defer s.sessions.Done()
		if req.Type == proto.TypeExec {
			runExecSession(c, req, s)
		} else {
			runConnectSession(c, req, s)
		}
	}()
	return true, nil
}

func (s *supervisorState) quiesce() error {
	s.sessionMu.Lock()
	s.sessionsPaused = true
	s.sessionMu.Unlock()
	s.closeRestartAdmission()
	s.pluginReg.beginQuiesce()
	if err := quiesceExecSessions(s.execReg); err != nil {
		return err
	}
	closeConnectSessions(s.connReg, s.acceptLn)
	s.sessions.Wait()
	s.bridge.closeLiveMUX()
	return nil
}

func (s *supervisorState) prepareReattach() error {
	s.bridge.closeLiveMUX()
	if err := resumeAfterThaw(s, nil); err != nil {
		return err
	}
	s.sessionMu.Lock()
	s.sessionsPaused = false
	s.sessionMu.Unlock()
	return nil
}

func (s *supervisorState) attachSession(c *vsockConn) (bool, error) {
	_ = c.SetDeadline(time.Time{})
	s.bridge.reattach(c)
	return true, nil
}

func primaryOuterPID(pid int) (int, error) {
	if innerEvents == nil {
		return 0, errors.New("primary must be launched by inner init")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, fmt.Errorf("open primary pidfd: %w", err)
	}
	defer unix.Close(fd)
	msg, err := innerEvents.request(executorMessage{Op: "identity"}, fd)
	if err != nil {
		return 0, fmt.Errorf("resolve outer primary PID: %w", err)
	}
	return msg.PID, nil
}
