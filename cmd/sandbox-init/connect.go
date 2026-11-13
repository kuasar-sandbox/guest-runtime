package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/fwd"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// The guest side of `sandbox-ctl run --connect` port forwarding: for each
// host-accepted local connection the host opens one reverse-channel conn
// carrying `connect{ConnectSpec}`; this side obtains the requested guest-side
// connection — `net.Dial` for dial mode (`LOCAL:TARGET`) or one Accept on a
// lazily-created listener for accept mode (`LOCAL::TARGET`) — acks, then
// splices the conn to it via the fwd frame sub-protocol (pkg/fwd), which
// preserves TCP half-close. Sessions are concurrent (one goroutine per
// reverse conn) and independent of the app and of each other — the
// port-forward analogue of exec sessions.

const (
	// connectDialTimeout bounds the guest-side dial so a slow/blocked
	// target can't pin the handshake (must stay under proto.DeadlineConnect).
	// Dial mode only — accept mode parks (the host waits without a deadline).
	connectDialTimeout = 5 * time.Second
	// connectLingerSec arms SO_LINGER on a forward's vsock conn at quiesce
	// so Close blocks until the host's RST removes the socket — no half-open
	// remnant survives into the snapshot (mirrors the stdio MUX teardown).
	connectLingerSec = 3
)

// errAcceptQuiescing is returned by acceptListeners.getOrCreate when a
// snapshot is in progress, so a racing accept-mode forward is refused rather
// than binding a listener that would outlive the quiesce teardown.
var errAcceptQuiescing = errors.New("sandbox quiescing (snapshot in progress)")

// connSession is one port-forward session. c is retained so quiesce can arm
// SO_LINGER before the conn closes. relay is set once the session is
// relaying; it stays nil while an accept-mode (`LOCAL::TARGET`) session is
// parked in Accept waiting for a guest-side client. closeOnce guards the
// parked-conn close so a quiesce teardown and the handler's own abort can't
// double-close c.
type connSession struct {
	relay     *fwd.Relay
	c         *vsockConn
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// teardown collapses the session with SO_LINGER armed so the vsock close is
// confirmed before a snapshot (no half-open remnant). A relaying session
// shuts its relay (which owns c); a parked accept session (relay == nil)
// closes c directly. Idempotent: sync.Once / relay.Shutdown each block all
// callers until the close completes, so closeConnectSessions can wg.Wait on
// it. relay is read after the registry mutex has serialized any promote, so
// the nil check is stable here.
func (s *connSession) teardown() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.relay != nil {
		s.relay.Shutdown(func() { _ = s.c.SetLinger(connectLingerSec) })
		return
	}
	s.closeOnce.Do(func() {
		_ = s.c.SetLinger(connectLingerSec)
		_ = s.c.Close()
	})
}

// connRegistry tracks live connect (port-forward) sessions so the quiesce
// handler can tear them all down before a snapshot — the port-forward
// analogue of execRegistry. A forward left open across a snapshot would be
// captured as a half-open vsock remnant (see vsockConn.SetLinger).
// restore / attach call endQuiesce to re-enable forwarding on the resumed
// sandbox.
type connRegistry struct {
	mu        sync.Mutex
	live      map[*connSession]struct{}
	quiescing bool
}

func newConnRegistry() *connRegistry {
	return &connRegistry{live: make(map[*connSession]struct{})}
}

// add registers a live session, returning false if the sandbox is
// quiescing (the caller must then tear the session down instead of acking).
func (r *connRegistry) add(s *connSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quiescing {
		return false
	}
	r.live[s] = struct{}{}
	return true
}

func (r *connRegistry) remove(s *connSession) {
	r.mu.Lock()
	delete(r.live, s)
	r.mu.Unlock()
}

// promote attaches a relay to a parked accept-mode session, returning false
// if the sandbox started quiescing while the session was parked in Accept
// (the caller must then tear the session down instead of acking). Setting
// s.relay under the registry mutex orders it against beginQuiesce, so a
// session's relay field is stable once beginQuiesce has snapshotted it.
func (r *connRegistry) promote(s *connSession, relay *fwd.Relay) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quiescing {
		return false
	}
	s.relay = relay
	return true
}

func (r *connRegistry) isQuiescing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quiescing
}

// beginQuiesce marks the sandbox quiescing (new connect rejected) and
// returns the live sessions to tear down.
func (r *connRegistry) beginQuiesce() []*connSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quiescing = true
	out := make([]*connSession, 0, len(r.live))
	for s := range r.live {
		out = append(out, s)
	}
	return out
}

func (r *connRegistry) endQuiesce() {
	r.mu.Lock()
	r.quiescing = false
	r.mu.Unlock()
}

// acceptListeners caches the guest-side listeners created for accept-mode
// (`LOCAL::TARGET`) forwards. A listener is created lazily on the first
// connect{accept} for an address and reused for every subsequent accept on
// it; quiesce closes them all (unblocking any parked Accept) and clears the
// cache, and they are recreated lazily after restore. Keyed by
// network+"\x00"+address. The quiescing flag closes the create-vs-quiesce
// race: getOrCreate holds mu across the check + bind + cache, and closeAll
// holds mu to set the flag + close + clear — so a listener is either fully
// created (and then closed by closeAll) or refused, never leaked past quiesce.
type acceptListeners struct {
	mu        sync.Mutex
	m         map[string]net.Listener
	quiescing bool
}

func newAcceptListeners() *acceptListeners {
	return &acceptListeners{m: make(map[string]net.Listener)}
}

// getOrCreate returns the cached listener for (network, address), binding and
// caching it on first use. Returns an error if the sandbox is quiescing or
// the bind fails. A stale node is removed first for a non-abstract unix path.
func (a *acceptListeners) getOrCreate(network, address string) (net.Listener, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quiescing {
		return nil, errAcceptQuiescing
	}
	key := network + "\x00" + address
	if ln, ok := a.m[key]; ok {
		return ln, nil
	}
	if network == "unix" && !strings.HasPrefix(address, "@") {
		_ = os.Remove(address)
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	a.m[key] = ln
	return ln, nil
}

// closeAll closes every cached listener (unblocking parked Accept calls),
// clears the cache, and marks quiescing so no new listener is bound until
// reopen. Called from closeConnectSessions at quiesce.
func (a *acceptListeners) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quiescing = true
	for k, ln := range a.m {
		_ = ln.Close()
		delete(a.m, k)
	}
}

// reopen re-enables lazy listener creation after a resume (restore / attach).
func (a *acceptListeners) reopen() {
	a.mu.Lock()
	a.quiescing = false
	a.mu.Unlock()
}

// closeConnectSessions marks the sandbox quiescing and tears down every
// port-forward session: each vsock conn is closed with SO_LINGER armed so
// Close blocks until the host's RST removes the socket — no half-open
// remnant survives into the snapshot. It also closes the cached accept-mode
// listeners, which unblocks any session parked in Accept (its session is in
// `sessions` and lingered below) and clears the bound addresses so the
// snapshot captures none; they rebind lazily after resume. Sessions are
// closed concurrently so the aggregate stays within the quiesce deadline.
// Called by the quiesce handler, alongside the exec-session drain and stdio
// MUX close.
func closeConnectSessions(reg *connRegistry, lns *acceptListeners) {
	sessions := reg.beginQuiesce()
	// Shutdown each parked reverse socket before waiting for listener Close:
	// acceptForPeer pins the shared listener while polling it and that socket.
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *connSession) {
			defer wg.Done()
			s.teardown()
		}(s)
	}
	lns.closeAll()
	wg.Wait()
}

type connectDialFunc func(context.Context, string, string) (net.Conn, error)

// runConnectSession services one proto.TypeConnect reverse-channel conn:
// obtain the guest-side connection (dial the target, or — in accept mode —
// Accept one on a lazily-created listener), ack, then splice the conn to it
// via the fwd frame relay (TCP half-close preserved). It owns c (the relay
// closes it). Blocks until the session ends.
func runConnectSession(c *vsockConn, req *proto.Message, sup *supervisorState) {
	dialer := &net.Dialer{Timeout: connectDialTimeout}
	runConnectSessionWithDial(c, req, sup, dialer.DialContext)
}

func runConnectSessionWithDial(c *vsockConn, req *proto.Message, sup *supervisorState, dial connectDialFunc) {
	reg := sup.connReg
	fail := func(msg string) {
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: msg})
		_ = c.Close()
	}

	spec := req.Connect
	if spec == nil || spec.Address == "" {
		fail("connect: empty address")
		return
	}
	network := spec.Network
	if network == "" {
		network = "tcp"
	}
	if spec.Accept {
		runAcceptSession(c, network, spec.Address, sup)
		return
	}

	// Register before the potentially blocking target dial. Quiesce can now
	// cancel the dial and perform the lingered reverse-connection close before
	// replying `quiesced`; a late dial completion cannot emit teardown afterward.
	dialCtx, cancel := context.WithCancel(context.Background())
	s := &connSession{c: c, cancel: cancel}
	if !reg.add(s) {
		s.teardown()
		return
	}
	defer func() {
		cancel()
		reg.remove(s)
	}()

	target, err := dial(dialCtx, network, spec.Address)
	if err != nil {
		if !reg.isQuiescing() {
			_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "connect: dial " + spec.Address + ": " + err.Error()})
		}
		s.teardown()
		return
	}
	cancel()

	relay := fwd.NewRelay(c, target)
	if !reg.promote(s, relay) {
		// Quiesce snapshotted this registered pre-dial session. Its teardown
		// owns the reverse conn; only dispose of the newly opened target here.
		_ = target.Close()
		s.teardown()
		return
	}

	if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeConnectAck}); err != nil {
		logf("connect: write connect_ack (%s): %v", spec.Address, err)
		s.teardown()
		return
	}
	_ = c.SetDeadline(time.Time{}) // hand off to the relay; no per-frame deadline

	logf("connect: established → %s", spec.Address)
	relay.Run()
	logf("connect: session done → %s", spec.Address)
}

// acceptForPeer waits for this host and the shared listener together. A
// disconnected host relinquishes its request without accepting a client or
// disturbing other waiters on the same address. The listener is nonblocking:
// another request can consume readiness between Poll and Accept4.
func acceptForPeer(ln net.Listener, peer *vsockConn) (net.Conn, error) {
	// getOrCreate only returns the system TCP and Unix listeners from net.Listen.
	raw, err := ln.(syscall.Conn).SyscallConn()
	if err != nil {
		return nil, err
	}
	var accepted int
	var acceptErr error
	err = raw.Control(func(listenerFD uintptr) {
		// rawIO retains the peer fd until poll returns. Close performs shutdown
		// before joining its users, including when quiesce cancels this wait.
		accepted, acceptErr = peer.rawIO(func(hostFD int, _ []byte) (int, error) {
			fds := []unix.PollFd{
				{Fd: int32(hostFD), Events: unix.POLLRDHUP},
				{Fd: int32(listenerFD), Events: unix.POLLIN},
			}
			for {
				_, err := unix.Poll(fds, -1)
				if err == unix.EINTR {
					continue
				}
				if err != nil {
					return -1, err
				}
				// Prefer cancellation if a client arrived after the host left.
				if fds[0].Revents != 0 {
					return -1, net.ErrClosed
				}
				fd, _, err := unix.Accept4(int(listenerFD), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
				if err == unix.EAGAIN || err == unix.EINTR || err == unix.ECONNABORTED {
					continue
				}
				return fd, err
			}
		}, nil)
	})
	if err != nil {
		return nil, err
	}
	if acceptErr != nil {
		return nil, acceptErr
	}
	file := os.NewFile(uintptr(accepted), ln.Addr().String())
	defer file.Close()
	return net.FileConn(file)
}

// runAcceptSession services an accept-mode (`LOCAL::TARGET`) connect: it
// Listen+Accepts one guest-side connection on (network, address) — via a
// lazily-created, cached listener — pairs it with this reverse-channel conn,
// acks, then relays. The Accept can block until a guest-side client connects,
// so the parked session is registered BEFORE Accept: a snapshot starting now
// closes the listener (unblocking Accept) and lingers the conn. It owns c.
func runAcceptSession(c *vsockConn, network, address string, sup *supervisorState) {
	reg := sup.connReg
	// Register the parked session before binding/accepting so a snapshot
	// starting now either observes it (tears it down + closes the listener)
	// or is observed here (add fails). relay is nil until promote below.
	s := &connSession{c: c}
	if !reg.add(s) {
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "connect: sandbox quiescing (snapshot in progress)"})
		s.teardown()
		return
	}

	defer reg.remove(s)

	ln, err := sup.acceptLn.getOrCreate(network, address)
	if err != nil {
		if !reg.isQuiescing() {
			_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "connect: listen " + address + ": " + err.Error()})
		}
		s.teardown()
		return
	}

	target, err := acceptForPeer(ln, c)
	if err != nil {
		if !errors.Is(err, net.ErrClosed) && !reg.isQuiescing() {
			_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "connect: accept " + address + ": " + err.Error()})
		}
		s.teardown()
		return
	}

	relay := fwd.NewRelay(c, target)
	if !reg.promote(s, relay) {
		// Raced with quiesce after Accept returned: don't ack. Close the just-
		// accepted guest conn and linger the reverse conn.
		_ = target.Close()
		s.teardown()
		return
	}

	if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeConnectAck}); err != nil {
		logf("connect(accept): write connect_ack (%s): %v", address, err)
		s.teardown()
		return
	}
	_ = c.SetDeadline(time.Time{}) // hand off to the relay; no per-frame deadline

	logf("connect(accept): established ← %s", address)
	relay.Run()
	logf("connect(accept): session done ← %s", address)
}
