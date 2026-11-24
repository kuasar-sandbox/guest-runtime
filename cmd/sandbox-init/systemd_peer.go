package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type systemdEnvironment struct {
	installation  *systemdInstallation
	token         string
	executor      *os.Process
	executorPIDFD int
	innerPID      int
	attached      chan error
	ready         chan struct{}
	mu            sync.Mutex
	peer          *systemdPeer
	nextRefresh   uint64
	refresh       map[uint64]chan int
	observer      atomic.Pointer[systemdObserver]
}

type systemdPeer struct {
	conn *net.UnixConn
	bus  *dbus.Conn
}

func unixPeer(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err = raw.Control(func(fd uintptr) { cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return nil, err
	}
	return cred, sockErr
}

// The connector runs in the manager's PID namespace. Passing the raw connected
// FD preserves the credentials systemd saw; outer must not connect it itself.
func connectSystemdManager(ctx context.Context) (int, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/run/systemd/private")
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	file, err := conn.(*net.UnixConn).File()
	if err != nil {
		return -1, err
	}
	defer file.Close()
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	return fd, err
}

func openSystemdPeer(ctx context.Context, fd, managerPID int) (*systemdPeer, error) {
	file := os.NewFile(uintptr(fd), "systemd-private")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	u, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("systemd peer is not a Unix socket")
	}
	cred, err := unixPeer(u)
	if err != nil || cred.Pid != int32(managerPID) || cred.Uid != 0 {
		_ = u.Close()
		return nil, fmt.Errorf("systemd manager peer mismatch: %v (%v)", cred, err)
	}
	if d, ok := ctx.Deadline(); ok {
		_ = u.SetDeadline(d)
	}
	stream := &systemdAuthConn{Conn: u}
	bus, err := dbus.NewConn(stream)
	if err != nil {
		_ = u.Close()
		return nil, err
	}
	// No Hello: /run/systemd/private is a direct peer, not a message bus.
	if err = bus.Auth([]dbus.Auth{dbus.AuthExternal("0")}); err != nil {
		_ = bus.Close()
		return nil, err
	}
	return &systemdPeer{conn: u, bus: bus}, nil
}

func (p *systemdPeer) close() {
	if p != nil {
		_ = p.bus.Close()
	}
}
func (p *systemdPeer) deadline(ctx context.Context) {
	d, _ := ctx.Deadline()
	_ = p.conn.SetDeadline(d)
}
func (p *systemdPeer) property(ctx context.Context, path dbus.ObjectPath, iface, name string) (dbus.Variant, error) {
	var value dbus.Variant
	err := p.bus.Object("org.freedesktop.systemd1", path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&value)
	return value, err
}
func (p *systemdPeer) properties(ctx context.Context, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	var props map[string]dbus.Variant
	err := p.bus.Object("org.freedesktop.systemd1", path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface).Store(&props)
	return props, err
}
func (p *systemdPeer) unit(ctx context.Context, name string) (dbus.ObjectPath, error) {
	var path dbus.ObjectPath
	err := p.bus.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx, "org.freedesktop.systemd1.Manager.GetUnit", 0, name).Store(&path)
	return path, err
}
func (p *systemdPeer) state(ctx context.Context) (string, error) {
	p.deadline(ctx)
	v, err := p.property(ctx, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "SystemState")
	if err != nil {
		return "", err
	}
	state, ok := v.Value().(string)
	if !ok || state == "" {
		return "", errors.New("invalid manager SystemState")
	}
	_ = p.conn.SetDeadline(time.Time{})
	return state, nil
}

func innerPIDForOuter(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if s, ok := strings.CutPrefix(line, "NSpid:"); ok {
			pids := strings.Fields(s)
			if len(pids) < 2 {
				return 0, errors.New("executor is not in an inner PID namespace")
			}
			return strconv.Atoi(pids[len(pids)-1])
		}
	}
	return 0, errors.New("executor namespace PID is unavailable")
}

func verifySystemdIdentity(manager, executor int) error {
	for _, ns := range []string{"pid", "mnt", "cgroup"} {
		a, err := os.Stat(fmt.Sprintf("/proc/%d/ns/%s", manager, ns))
		if err != nil {
			return err
		}
		b, err := os.Stat(fmt.Sprintf("/proc/%d/ns/%s", executor, ns))
		if err != nil {
			return err
		}
		if !os.SameFile(a, b) {
			return fmt.Errorf("systemd executor has a different %s namespace", ns)
		}
	}
	a, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	b, err := os.Stat(fmt.Sprintf("/proc/%d/exe", executor))
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return errors.New("systemd executor executable identity differs from Runtime")
	}
	return nil
}

func (c *executorClient) acceptSystemdExecutor(conn *net.UnixConn) (resultErr error) {
	e := c.systemd
	cred, err := unixPeer(conn)
	if err != nil {
		return err
	}
	if cred.Uid != 0 || cred.Pid <= 0 {
		return errors.New("systemd executor peer must be root with a live PID")
	}
	logf("systemd executor connection pid=%d", cred.Pid)
	fd, err := unix.PidfdOpen(int(cred.Pid), 0)
	if err != nil {
		return fmt.Errorf("pin systemd executor: %w", err)
	}
	e.executorPIDFD = fd
	defer func() {
		if resultErr != nil {
			_ = unix.Close(fd)
		}
	}()
	if err = verifySystemdIdentity(c.process.Pid, int(cred.Pid)); err != nil {
		return err
	}
	e.innerPID, err = innerPIDForOuter(int(cred.Pid))
	if err != nil {
		return err
	}
	e.executor, err = os.FindProcess(int(cred.Pid))
	if err != nil {
		return err
	}
	e.attached = make(chan error, 1)
	e.ready = make(chan struct{})
	c.link = newExecutorLink(conn)
	go c.link.serve(c.handleEvent)
	select {
	case err = <-e.attached:
		if err != nil {
			return err
		}
	case <-c.link.done:
		return errors.New("systemd executor disconnected during verification")
	case <-c.processDone:
		return fmt.Errorf("systemd exited during verification: %v", c.processErr)
	}
	select {
	case <-e.ready:
		observer := newSystemdObserver(c)
		e.observer.Store(observer)
		observer.start()
		return nil
	case <-c.link.done:
		return errors.New("systemd executor disconnected before READY")
	case <-c.processDone:
		return fmt.Errorf("systemd exited before READY: %v", c.processErr)
	}
}

func (c *executorClient) attachSystemd(msg executorMessage, fd *int) error {
	e := c.systemd
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.peer != nil || msg.Token != e.token || msg.PID != e.innerPID || *fd < 0 {
		return errors.New("invalid or duplicate systemd executor attach")
	}
	// Initial attach shares Host's existing pre-ACK startup lifetime. There
	// is no new five-second cold-start timeout; observer budgets begin only
	// after this first authenticated, valid manager observation.
	ctx := context.Background()
	owned := *fd
	*fd = -1
	logf("systemd executor attach received; authenticating manager peer")
	peer, err := openSystemdPeer(ctx, owned, c.process.Pid)
	if err == nil {
		logf("systemd manager peer authenticated")
	}
	if err == nil {
		err = c.validateSystemdService(ctx, peer)
	}
	if err == nil {
		logf("systemd executor effective service verified")
	}
	if err == nil {
		var state string
		state, err = peer.state(ctx)
		if err == nil && state == "stopping" {
			err = errors.New("systemd already stopping during attach")
		}
	}
	if err != nil {
		peer.close()
		e.attached <- err
		return err
	}
	e.peer = peer
	e.attached <- nil
	return nil
}

// sd_notify is a datagram. Sending READY does not establish that the manager
// has processed it. Confirm active before allowing Init to call systemctl,
// otherwise an Init job can still wait behind its own activating service.
func (c *executorClient) confirmSystemdReady() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c.processDone:
			cancel()
		case <-c.link.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	peer := c.systemd.peer
	path, err := peer.unit(ctx, systemdService)
	if err != nil {
		return err
	}
	for {
		value, err := peer.property(ctx, path, "org.freedesktop.systemd1.Unit", "ActiveState")
		if err != nil {
			return err
		}
		switch value.Value() {
		case "active":
			logf("systemd executor service active")
			return nil
		case "activating":
		default:
			return fmt.Errorf("executor service did not become ready: %v", value.Value())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func requireProperty(props map[string]dbus.Variant, name string, want any) error {
	v, ok := props[name]
	if !ok || !reflect.DeepEqual(v.Value(), want) {
		return fmt.Errorf("reserved unit property %s=%v, require %v", name, v.Value(), want)
	}
	return nil
}
func propertyStrings(props map[string]dbus.Variant, name string) []string {
	v, _ := props[name].Value().([]string)
	return v
}
func containsString(values []string, want string) bool {
	for _, s := range values {
		if s == want {
			return true
		}
	}
	return false
}

func validateReservedUnit(props map[string]dbus.Variant, name string) error {
	for key, want := range map[string]any{"LoadState": "loaded", "DefaultDependencies": false, "IgnoreOnIsolate": true, "RefuseManualStop": true} {
		if err := requireProperty(props, key, want); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (c *executorClient) validateSystemdService(ctx context.Context, p *systemdPeer) error {
	e := c.systemd
	logf("systemd verification: manager Version")
	v, err := p.property(ctx, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version")
	if err != nil {
		return err
	}
	version, ok := v.Value().(string)
	if !ok {
		return errors.New("invalid manager version")
	}
	digits := strings.TrimLeft(version, "v")
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	major, err := strconv.Atoi(digits)
	if err != nil || major < 252 {
		return fmt.Errorf("systemd version %q is below the mechanism floor 252", version)
	}
	logf("systemd verification: executor GetUnit")
	path, err := p.unit(ctx, systemdService)
	if err != nil {
		return err
	}
	logf("systemd verification: executor Unit properties")
	u, err := p.properties(ctx, path, "org.freedesktop.systemd1.Unit")
	if err != nil {
		return err
	}
	if err := validateReservedUnit(u, systemdService); err != nil {
		return err
	}
	if err := requireProperty(u, "RefuseManualStart", true); err != nil {
		return err
	}
	if !containsString(propertyStrings(u, "Conflicts"), "shutdown.target") || !containsString(propertyStrings(u, "Before"), "shutdown.target") || !containsString(propertyStrings(u, "Before"), "umount.target") {
		return errors.New("reserved executor service lost shutdown ordering")
	}
	logf("systemd verification: executor Service properties")
	s, err := p.properties(ctx, path, "org.freedesktop.systemd1.Service")
	if err != nil {
		return err
	}
	for key, want := range map[string]any{"Type": "notify", "NotifyAccess": "main", "Restart": "no", "KillMode": "mixed", "Delegate": true, "TasksMax": uint64(math.MaxUint64), "OOMPolicy": "continue", "TimeoutStartUSec": uint64(math.MaxUint64), "TimeoutStopUSec": uint64((systemdGrace(c.spec) + 5*time.Second) / time.Microsecond), "MainPID": uint32(e.innerPID)} {
		if err := requireProperty(s, key, want); err != nil {
			return err
		}
	}
	// Properties may differ in type between supported systemd generations.
	// The actual namespaces and process identity are checked independently.
	for _, key := range []string{"PrivateTmp", "PrivateDevices", "PrivateNetwork", "PrivateUsers", "PrivateMounts", "ProtectControlGroups", "ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs", "MountAPIVFS"} {
		if v, ok := s[key]; ok && v.Value() != false && v.Value() != "no" {
			return fmt.Errorf("reserved executor service %s=%v changes its environment", key, v.Value())
		}
	}
	for _, key := range []string{"RootDirectory", "RootImage", "ProtectSystem", "ProtectHome", "User", "Group"} {
		if v, ok := s[key]; ok {
			value, _ := v.Value().(string)
			if value != "" && value != "no" && !((key == "User" || key == "Group") && (value == "root" || value == "0")) {
				return fmt.Errorf("reserved executor service %s=%v", key, v.Value())
			}
		}
	}
	for _, key := range []string{"BindPaths", "BindReadOnlyPaths", "ReadWritePaths", "ReadOnlyPaths", "InaccessiblePaths", "TemporaryFileSystem", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		if v, ok := s[key]; ok {
			rv := reflect.ValueOf(v.Value())
			if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && rv.Len() != 0 {
				return fmt.Errorf("reserved executor service %s must be empty", key)
			}
		}
	}
	type startCommand struct {
		Path                                     string
		Args                                     []string
		Ignore                                   bool
		StartReal, StartMono, ExitReal, ExitMono uint64
		PID                                      uint32
		Code, Status                             int32
	}
	var commands []startCommand
	if err := dbus.Store([]any{s["ExecStart"].Value()}, &commands); err != nil {
		return fmt.Errorf("reserved ExecStart: %w", err)
	}
	groups := "0"
	if len(guestSlots.disks) != 0 {
		groups = "1"
	}
	flavor := "provided"
	if e.installation.runtime {
		flavor = "runtime"
	}
	if len(commands) != 1 || commands[0].Path != systemdExecutorPath || !reflect.DeepEqual(commands[0].Args, []string{systemdExecutorPath, "systemd-executor", e.token, groups, flavor}) || commands[0].Ignore {
		return errors.New("reserved executor ExecStart was changed")
	}
	cgroup, ok := s["ControlGroup"].Value().(string)
	if !ok || cgroup == "/" || !filepath.IsAbs(cgroup) || filepath.Clean(cgroup) != cgroup {
		return fmt.Errorf("invalid service ControlGroup %q", cgroup)
	}
	procs, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", e.executor.Pid))
	if err != nil {
		return err
	}
	// outer has the parent cgroup namespace; manager paths are rooted at
	// /sandbox. Verify against kernel ownership, never a hard-coded slice.
	if !strings.Contains(string(procs), "0::/sandbox"+cgroup+"/init\n") {
		return fmt.Errorf("executor cgroup differs from manager ControlGroup %s: %s", cgroup, procs)
	}
	if err := verifySystemdIdentity(c.process.Pid, e.executor.Pid); err != nil {
		return err
	}
	logf("systemd verification: default target")
	defaultPath, err := p.unit(ctx, "default.target")
	if err != nil {
		return fmt.Errorf("required default.target: %w", err)
	}
	defaultProps, err := p.properties(ctx, defaultPath, "org.freedesktop.systemd1.Unit")
	if err != nil {
		return err
	}
	defaultNames := propertyStrings(defaultProps, "Names")
	for _, name := range append(append([]string{}, e.installation.protected...), systemdTarget, "default.target") {
		logf("systemd verification: protected unit %s", name)
		upath, err := p.unit(ctx, name)
		if err != nil {
			return fmt.Errorf("required unit %s: %w", name, err)
		}
		props, err := p.properties(ctx, upath, "org.freedesktop.systemd1.Unit")
		if err != nil {
			return err
		}
		if err := requireProperty(props, "LoadState", "loaded"); err != nil {
			return fmt.Errorf("required unit %s: %w", name, err)
		}
		if name == systemdTarget {
			if !containsString(propertyStrings(props, "Requires"), systemdService) || !containsString(propertyStrings(props, "After"), systemdService) {
				return errors.New("reserved target lost executor dependency")
			}
			if err := requireProperty(props, "DefaultDependencies", false); err != nil {
				return err
			}
			requiredDefault := false
			for _, alias := range defaultNames {
				if containsString(propertyStrings(props, "Requires"), alias) {
					requiredDefault = true
				}
				if containsString(propertyStrings(props, "After"), alias) {
					return errors.New("reserved target must not wait for default.target")
				}
			}
			if !requiredDefault {
				return errors.New("reserved target lost default.target dependency")
			}
		} else if name != "default.target" {
			if err := validateReservedUnit(props, name); err != nil {
				return err
			}
			if len(propertyStrings(props, "Conflicts")) != 0 {
				return fmt.Errorf("protected mount %s has conflicting units: %v", name, propertyStrings(props, "Conflicts"))
			}
			if err := requireProperty(props, "StopWhenUnneeded", false); err != nil {
				return fmt.Errorf("protected mount %s: %w", name, err)
			}
		}
	}
	logf("systemd verified version=%s MainPID=%d ControlGroup=%s", version, e.innerPID, cgroup)
	return nil
}

// Refresh uses its own epoch, not executorLink.call: a late manager response
// must be discarded without closing the execution channel or replaying Launch.
func (c *executorClient) refreshSystemdPeer(ctx context.Context) (*systemdPeer, error) {
	e := c.systemd
	e.mu.Lock()
	e.nextRefresh++
	epoch := e.nextRefresh
	ch := make(chan int, 1)
	e.refresh[epoch] = ch
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.refresh, epoch)
		select {
		case fd := <-ch:
			if fd >= 0 {
				_ = unix.Close(fd)
			}
		default:
		}
		e.mu.Unlock()
	}()
	if err := c.link.sendControl(ctx, executorMessage{Op: "systemd-peer", Epoch: epoch}, -1); err != nil {
		return nil, err
	}
	select {
	case fd := <-ch:
		if fd < 0 {
			return nil, errors.New("executor could not connect to systemd manager")
		}
		return openSystemdPeer(ctx, fd, c.process.Pid)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.link.done:
		return nil, errors.New("executor link closed")
	}
}

func (e *systemdEnvironment) receivePeer(msg executorMessage, fd *int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.refresh[msg.Epoch]
	if ch == nil {
		return
	}
	select {
	case ch <- *fd:
		*fd = -1
	default:
	}
}
