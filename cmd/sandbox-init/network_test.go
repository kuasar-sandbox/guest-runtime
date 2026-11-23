package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

type platformTestRequest struct {
	kind, flags uint16
	body        []byte
}

type platformTestConn struct {
	calls []platformTestRequest
	run   func(platformTestRequest) ([]networkReply, error)
}

func (c *platformTestConn) request(ctx context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := platformTestRequest{kind, flags, append([]byte(nil), body...)}
	c.calls = append(c.calls, r)
	if c.run != nil {
		return c.run(r)
	}
	return nil, nil
}

func TestPlatformReplacementOwnsOnlyItsDefaultAndGlobalAddresses(t *testing.T) {
	oldRoute := platformDefaultRoute(7, unix.AF_INET, net.ParseIP("192.0.2.1"))
	p := platformNetwork{index: 7, defaultRoute: append([]byte(nil), oldRoute...)}
	old := []platformAddress{
		{family: unix.AF_INET, ip: net.ParseIP("192.0.2.2"), prefix: 24, scope: unix.RT_SCOPE_UNIVERSE},
		{family: unix.AF_INET6, ip: net.ParseIP("2001:db8::2"), prefix: 64, scope: unix.RT_SCOPE_UNIVERSE},
		{family: unix.AF_INET6, ip: net.ParseIP("fe80::2"), prefix: 64, scope: unix.RT_SCOPE_LINK},
	}
	c := &platformTestConn{}
	cfg, err := parsePlatformConfig(&proto.NetworkSpec{IPCIDR: "198.51.100.2/24"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.configure(context.Background(), c, 7, cfg, old, true); err != nil {
		t.Fatal(err)
	}
	wantKinds := []uint16{unix.RTM_NEWLINK, unix.RTM_DELROUTE, unix.RTM_DELADDR, unix.RTM_DELADDR, unix.RTM_NEWADDR}
	if len(c.calls) != len(wantKinds) {
		t.Fatalf("unexpected writes: %+v", c.calls)
	}
	for i, want := range wantKinds {
		if c.calls[i].kind != want {
			t.Fatalf("write %d kind %d, want %d", i, c.calls[i].kind, want)
		}
	}
	if !bytes.Equal(c.calls[1].body, oldRoute) || p.defaultRoute != nil {
		t.Fatal("empty gateway did not remove exactly the owned default")
	}
	if c.calls[2].body[0] != unix.AF_INET || c.calls[3].body[0] != unix.AF_INET6 {
		t.Fatal("replacement retained the old address family")
	}
	if c.calls[4].flags&unix.NLM_F_REPLACE != 0 || c.calls[4].flags&unix.NLM_F_EXCL == 0 {
		t.Fatal("address creation may replace an unrelated entry")
	}
}

func TestPlatformDefaultOwnershipChangesOnlyOnAcknowledgment(t *testing.T) {
	for _, phase := range []string{"delete", "add"} {
		t.Run(phase, func(t *testing.T) {
			oldRoute := platformDefaultRoute(7, unix.AF_INET, net.ParseIP("192.0.2.1"))
			p := platformNetwork{defaultRoute: append([]byte(nil), oldRoute...)}
			cfg, _ := parsePlatformConfig(&proto.NetworkSpec{IPCIDR: "2001:db8::2/64", Nexthop: "2001:db8::1"})
			c := &platformTestConn{run: func(r platformTestRequest) ([]networkReply, error) {
				if phase == "delete" && r.kind == unix.RTM_DELROUTE || phase == "add" && r.kind == unix.RTM_NEWROUTE {
					return nil, unix.EPERM
				}
				return nil, nil
			}}
			if err := p.configure(context.Background(), c, 7, cfg, nil, true); !errors.Is(err, unix.EPERM) {
				t.Fatal(err)
			}
			if phase == "delete" && (!bytes.Equal(p.defaultRoute, oldRoute) || len(c.calls) != 2) {
				t.Fatal("failed deletion lost ownership or proceeded to add another default")
			}
			if phase == "add" && p.defaultRoute != nil {
				t.Fatal("unacknowledged route became platform-owned")
			}
		})
	}
}

func TestPlatformDefaultDeleteKeyHasNoWildcardMetric(t *testing.T) {
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		ip := net.ParseIP("192.0.2.1")
		if family == unix.AF_INET6 {
			ip = net.ParseIP("2001:db8::1")
		}
		body := platformDefaultRoute(7, family, ip)
		attrs, err := parseNetworkAttrs(body[unix.SizeofRtMsg:])
		if err != nil || len(attrs[unix.RTA_PRIORITY]) != 4 || binary.NativeEndian.Uint32(attrs[unix.RTA_PRIORITY]) == 0 {
			t.Fatalf("default deletion can wildcard the route metric: %x %v", body, err)
		}
	}
}

func TestPlatformConfigRejectsInvalidIdentityBeforeWrites(t *testing.T) {
	for _, spec := range []*proto.NetworkSpec{
		{IPCIDR: "bad"}, {IPCIDR: "192.0.2.2/24", Nexthop: "2001:db8::1"},
		{IPCIDR: "2001:db8::2/64", Nexthop: "192.0.2.1"},
		{IPCIDR: "::ffff:192.0.2.2/120"}, {IPCIDR: "192.0.2.2/24", MTU: -1},
	} {
		if _, err := parsePlatformConfig(spec); err == nil {
			t.Fatalf("accepted %+v", spec)
		}
	}
	for _, spec := range []*proto.NetworkSpec{nil, {}, {Hostname: "only-hostname"}} {
		if cfg, err := parsePlatformConfig(spec); cfg != nil || err != nil {
			t.Fatalf("empty identity changed configuration: %+v %v", cfg, err)
		}
	}
}

func platformTestNeighbor(index int32, family int, state uint16, flags byte, ip string) networkReply {
	body := make([]byte, unix.SizeofNdMsg)
	body[0], body[10] = byte(family), flags
	binary.NativeEndian.PutUint32(body[4:], uint32(index))
	binary.NativeEndian.PutUint16(body[8:], state)
	return networkReply{Type: unix.RTM_NEWNEIGH, Data: nlAttr(body, unix.NDA_DST, addrBytes(family, net.ParseIP(ip)))}
}

func TestPlatformNeighborsExcludeUserDevicesStaticProxyAndFDB(t *testing.T) {
	replies := []networkReply{
		platformTestNeighbor(7, unix.AF_INET, unix.NUD_STALE, 0, "192.0.2.1"),
		platformTestNeighbor(7, unix.AF_INET6, unix.NUD_REACHABLE, 0, "2001:db8::1"),
		platformTestNeighbor(8, unix.AF_INET, unix.NUD_STALE, 0, "198.51.100.1"),
		platformTestNeighbor(7, unix.AF_INET, unix.NUD_PERMANENT, 0, "192.0.2.3"),
		platformTestNeighbor(7, unix.AF_INET, unix.NUD_NOARP, 0, "192.0.2.4"),
		platformTestNeighbor(7, unix.AF_INET, unix.NUD_STALE, unix.NTF_PROXY, "192.0.2.5"),
		platformTestNeighbor(7, unix.AF_BRIDGE, unix.NUD_STALE, 0, "192.0.2.6"),
	}
	c := &platformTestConn{run: func(r platformTestRequest) ([]networkReply, error) {
		if r.kind == unix.RTM_GETNEIGH {
			return replies, nil
		}
		if len(r.body) <= unix.SizeofNdMsg || int32(binary.NativeEndian.Uint32(r.body[4:])) != 7 {
			t.Fatal("neighbor deletion escaped the platform link")
		}
		return nil, nil
	}}
	if err := flushPlatformNeighbors(context.Background(), c, 7); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 3 || c.calls[1].kind != unix.RTM_DELNEIGH || c.calls[2].kind != unix.RTM_DELNEIGH {
		t.Fatalf("unexpected neighbor writes: %+v", c.calls)
	}
	// Never apply a partial/malformed census, even when an earlier entry
	// looked valid. Dump failure must leave all candidate neighbors alone.
	replies = append(replies, networkReply{Type: unix.RTM_NEWNEIGH, Data: []byte{1}})
	c.calls = nil
	if err := flushPlatformNeighbors(context.Background(), c, 7); err == nil || len(c.calls) != 1 {
		t.Fatal("malformed dump led to partial deletion")
	}
}

func TestRestoreNetworkFailureStillThawsAndAcknowledges(t *testing.T) {
	for _, fault := range []error{unix.EOPNOTSUPP, unix.EPERM, unix.EIO, context.DeadlineExceeded} {
		t.Run(fault.Error(), func(t *testing.T) {
			_, sup, bridge := usageReattachFixture(t)
			guest, host := usageSocketPair(t)
			called, thawed := false, false
			done := make(chan struct{})
			var handed bool
			go func() {
				defer close(done)
				handed = handleReverseConnNetwork(guest, sup, func() {}, func(spec *proto.NetworkSpec) error {
					if spec != nil {
						t.Error("nil Restore identity was changed")
					}
					called = true
					return fault
				}, func() error {
					if !called {
						t.Error("network step happened after thaw")
					}
					thawed = true
					return nil
				})
			}()
			if err := proto.WriteMessage(host, &proto.Message{Type: proto.TypeRestore, Epoch: 144}); err != nil {
				t.Fatal(err)
			}
			ack, err := proto.ReadMessage(usageIO{host, time.Now().Add(time.Second)})
			waitUsageDone(t, done)
			t.Cleanup(func() { closeUsageTestMUX(t, bridge, host) })
			if err != nil || ack.Type != proto.TypeRestoreAck || ack.Epoch != 144 || !handed || !called || !thawed {
				t.Fatalf("fault blocked restore: ack=%+v err=%v handed=%v called=%v thawed=%v", ack, err, handed, called, thawed)
			}
			if sup.quiescing.Load() || guestMemReports.isPaused() {
				t.Fatal("successful ACK left execution or observation gates paused")
			}
		})
	}
}

type platformTestSession struct {
	networkRequester
	closed bool
}

func (s *platformTestSession) close() { s.closed = true }

func TestPlatformCoordinatorTCPFailuresDoNotEscapeOrSkipNetwork(t *testing.T) {
	for _, phase := range []string{"open", "dump", "timeout", "release"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _ := parsePlatformConfig(&proto.NetworkSpec{IPCIDR: "198.51.100.2/24"})
			var configured, neighbors bool
			config := &platformTestConn{run: func(r platformTestRequest) ([]networkReply, error) {
				switch r.kind {
				case unix.RTM_GETADDR:
					return []networkReply{{Type: unix.RTM_NEWADDR, Data: networkAddressBody(7, unix.AF_INET, net.ParseIP("192.0.2.2"), 24)}}, nil
				case unix.RTM_NEWADDR:
					configured = true
				case unix.RTM_GETNEIGH:
					neighbors = true
				}
				return nil, nil
			}}
			dump := tcpTestDump(t, tcpTestDiag(unix.AF_INET, "192.0.2.2", "203.0.113.1", 1, 1234))
			diag := &platformTestSession{networkRequester: tcpRequesterFunc(func(ctx context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
				if phase == "dump" {
					return nil, unix.EOPNOTSUPP
				}
				if phase == "timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if kind == platformSockDestroy {
					if !configured || !neighbors {
						t.Error("destroy preceded configuration/neighbor handling")
					}
					return nil, unix.EPERM
				}
				if configured {
					t.Error("TCP census ran after identity replacement")
				}
				return dump.request(ctx, kind, flags, body)
			})}
			route := &platformTestSession{networkRequester: tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
				return []networkReply{tcpTestRoute(unix.AF_INET, 7, unix.RTN_UNICAST)}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := resetPlatformNetwork(ctx, config, &platformNetwork{}, 7, cfg, nil, func(protocol int) (networkSession, error) {
				if protocol == unix.NETLINK_ROUTE {
					return route, nil
				}
				if phase == "open" {
					// A constructor returning *T may wrap a nil pointer in an
					// interface. Failed opens must never register its close.
					return (*platformTestSession)(nil), unix.EPROTONOSUPPORT
				}
				return diag, nil
			})
			if err != nil || !configured || !neighbors || ctx.Err() != nil {
				t.Fatalf("TCP fault affected configuration/restore: %v configured=%v neighbors=%v parent=%v", err, configured, neighbors, ctx.Err())
			}
			if !route.closed || phase != "open" && !diag.closed {
				t.Fatal("TCP socket outlived the restore attempt")
			}
		})
	}
}

func TestAttachDoesNotResetPlatformNetwork(t *testing.T) {
	_, sup, bridge := usageReattachFixture(t)
	guest, host := usageSocketPair(t)
	done := make(chan struct{})
	var handed bool
	go func() {
		defer close(done)
		handed = handleReverseConnNetwork(guest, sup, func() {}, func(*proto.NetworkSpec) error {
			t.Error("Attach invoked platform reset")
			return nil
		}, func() error { return nil })
	}()
	if err := proto.WriteMessage(host, &proto.Message{Type: proto.TypeAttach}); err != nil {
		t.Fatal(err)
	}
	ack, err := proto.ReadMessage(host)
	waitUsageDone(t, done)
	t.Cleanup(func() { closeUsageTestMUX(t, bridge, host) })
	if err != nil || ack.Type != proto.TypeAttachAck || !handed {
		t.Fatalf("Attach result %+v %v handed=%v", ack, err, handed)
	}
}
