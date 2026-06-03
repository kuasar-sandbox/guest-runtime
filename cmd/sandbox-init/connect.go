package main

import (
	"net"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/fwd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/proto"
)

// The guest side of `sandbox-ctl run --connect` port forwarding: for each
// host-accepted local connection the host opens one reverse-channel conn
// carrying `connect{ConnectSpec}`; this side dials the requested guest-side
// target, acks, then splices the conn to the target via the fwd frame
// sub-protocol (pkg/sandbox/fwd), which preserves TCP half-close. Sessions
// are concurrent (one goroutine per reverse conn) and independent of the
// app and of each other — the port-forward analogue of exec sessions.

const (
	// connectDialTimeout bounds the guest-side dial so a slow/blocked
	// target can't pin the handshake (must stay under proto.DeadlineConnect).
	connectDialTimeout = 5 * time.Second
	// connectLingerSec arms SO_LINGER on a forward's vsock conn at quiesce
	// so Close blocks until the host's RST removes the socket — no half-open
	// remnant survives into the snapshot (mirrors the stdio MUX teardown).
	connectLingerSec = 3
)

// connSession is one live port-forward relay. c is retained so quiesce can
// arm SO_LINGER before the relay closes it.
type connSession struct {
	relay *fwd.Relay
	c     *vsockConn
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

// closeConnectSessions marks the sandbox quiescing and tears down every
// live port-forward relay: each vsock conn is closed with SO_LINGER armed
// so Close blocks until the host's RST removes the socket — no half-open
// remnant survives into the snapshot. Sessions are closed concurrently so
// the aggregate stays within the quiesce deadline. Called by the quiesce
// handler, alongside killExecChildren + the stdio MUX close.
func closeConnectSessions(reg *connRegistry) {
	sessions := reg.beginQuiesce()
	if len(sessions) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *connSession) {
			defer wg.Done()
			s.relay.Shutdown(func() { _ = s.c.SetLinger(connectLingerSec) })
		}(s)
	}
	wg.Wait()
}

// runConnectSession services one proto.TypeConnect reverse-channel conn:
// dial the requested guest-side target, ack, then splice the conn to the
// target via the fwd frame relay (TCP half-close preserved). It owns c
// (the relay closes it). Blocks until the session ends.
func runConnectSession(c *vsockConn, req *proto.Message, sup *supervisorState) {
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
	// Reject new forwards while quiescing: a snapshot wants zero in-flight
	// forward connections, and the dial target (the app) is frozen, so a
	// dial would hang.
	if reg.isQuiescing() {
		fail("connect: sandbox quiescing (snapshot in progress)")
		return
	}
	network := spec.Network
	if network == "" {
		network = "tcp"
	}
	target, err := net.DialTimeout(network, spec.Address, connectDialTimeout)
	if err != nil {
		fail("connect: dial " + spec.Address + ": " + err.Error())
		return
	}

	relay := fwd.NewRelay(c, target)
	s := &connSession{relay: relay, c: c}
	// Register before acking so a snapshot starting now either observes
	// this session (and tears it down) or is observed here (add fails).
	if !reg.add(s) {
		// Raced with quiesce: don't ack — the host's OpenForward sees the
		// conn close and fails the local connection. Lingered teardown.
		relay.Shutdown(func() { _ = c.SetLinger(connectLingerSec) })
		return
	}
	defer reg.remove(s)

	if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeConnectAck}); err != nil {
		logf("connect: write connect_ack (%s): %v", spec.Address, err)
		relay.Shutdown(nil)
		return
	}
	_ = c.SetDeadline(time.Time{}) // hand off to the relay; no per-frame deadline

	logf("connect: established → %s", spec.Address)
	relay.Run()
	logf("connect: session done → %s", spec.Address)
}
