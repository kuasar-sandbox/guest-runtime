package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testNetworkMessage(kind, flags uint16, seq, port uint32, body []byte) []byte {
	length := unix.SizeofNlMsghdr + len(body)
	b := make([]byte, nlAlign(length))
	binary.NativeEndian.PutUint32(b, uint32(length))
	binary.NativeEndian.PutUint16(b[4:], kind)
	binary.NativeEndian.PutUint16(b[6:], flags)
	binary.NativeEndian.PutUint32(b[8:], seq)
	binary.NativeEndian.PutUint32(b[12:], port)
	copy(b[unix.SizeofNlMsghdr:], body)
	return b
}

func testNetworkStatus(code int32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, uint32(code))
	return b
}

func testNetworkACK(code int32) []byte {
	original := testNetworkMessage(unix.RTM_GETLINK, unix.NLM_F_REQUEST, 41, 97, nil)
	body := append(testNetworkStatus(code), original...)
	return testNetworkMessage(unix.NLMSG_ERROR, 0, 41, 97, body)
}

func testNetworkResponse() networkResponse {
	return networkResponse{
		seq: 41, port: 97, requestType: unix.RTM_GETLINK,
		requestLen: unix.SizeofNlMsghdr, requestFlags: unix.NLM_F_REQUEST,
	}
}

func TestNetworkDumpRequiresDone(t *testing.T) {
	s := testNetworkResponse()
	s.dump = true
	packet := testNetworkMessage(unix.RTM_NEWLINK, unix.NLM_F_MULTI, 41, 97, []byte{1, 2, 3})
	if done, err := s.consume(packet, 0); done || err != nil {
		t.Fatalf("partial dump completed: done=%v, err=%v", done, err)
	}
	// recvmsg reuses its buffer; accumulated replies must own their bytes.
	packet[unix.SizeofNlMsghdr] = 99
	if done, err := s.consume(testNetworkACK(0), 0); done || err != nil {
		t.Fatalf("ACK completed a dump: done=%v, err=%v", done, err)
	}
	done, err := s.consume(testNetworkMessage(unix.NLMSG_DONE, unix.NLM_F_MULTI, 41, 97, testNetworkStatus(0)), 0)
	if !done || err != nil {
		t.Fatalf("DONE failed: done=%v, err=%v", done, err)
	}
	if len(s.replies) != 1 || !bytes.Equal(s.replies[0].Data, []byte{1, 2, 3}) {
		t.Fatalf("dump data changed after receive buffer reuse: %+v", s.replies)
	}
}

func TestNetworkQueryDrainsRequestedACK(t *testing.T) {
	for _, ack := range []bool{false, true} {
		s := testNetworkResponse()
		s.wantACK = ack
		packet := testNetworkMessage(unix.RTM_NEWLINK, 0, 41, 0, []byte{1})
		done, err := s.consume(packet, 0)
		if err != nil || done == ack {
			t.Fatalf("query with ACK=%v: done=%v, err=%v", ack, done, err)
		}
		if ack {
			if done, err := s.consume(testNetworkACK(0), 0); !done || err != nil {
				t.Fatalf("query ACK not drained: done=%v, err=%v", done, err)
			}
		}
	}
}

func TestNetworkRejectsIncompleteOrMismatchedReplies(t *testing.T) {
	data := func() []byte { return testNetworkMessage(unix.RTM_NEWLINK, unix.NLM_F_MULTI, 41, 97, []byte{1}) }
	badLength := data()
	binary.NativeEndian.PutUint32(badLength, uint32(len(badLength)+1))
	shortLength := data()
	binary.NativeEndian.PutUint32(shortLength, 15)
	badEcho := testNetworkACK(0)
	binary.NativeEndian.PutUint32(badEcho[unix.SizeofNlMsghdr+4+8:], 42)
	badEchoType := testNetworkACK(0)
	binary.NativeEndian.PutUint16(badEchoType[unix.SizeofNlMsghdr+4+4:], unix.RTM_GETADDR)
	badEchoPort := testNetworkACK(0)
	binary.NativeEndian.PutUint32(badEchoPort[unix.SizeofNlMsghdr+4+12:], 98)
	badEchoLength := testNetworkACK(0)
	binary.NativeEndian.PutUint32(badEchoLength[unix.SizeofNlMsghdr+4:], unix.SizeofNlMsghdr+4)
	badEchoFlags := testNetworkACK(0)
	binary.NativeEndian.PutUint16(badEchoFlags[unix.SizeofNlMsghdr+4+6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	done := testNetworkMessage(unix.NLMSG_DONE, unix.NLM_F_MULTI, 41, 97, testNetworkStatus(0))
	tests := []struct {
		name   string
		packet []byte
		flags  int
	}{
		{"truncated", data(), unix.MSG_TRUNC},
		{"empty", nil, 0},
		{"short header", make([]byte, 15), 0},
		{"length beyond packet", badLength, 0},
		{"length shorter than header", shortLength, 0},
		{"wrong sequence", testNetworkMessage(unix.RTM_NEWLINK, 0, 42, 97, nil), 0},
		{"wrong port", testNetworkMessage(unix.RTM_NEWLINK, 0, 41, 98, nil), 0},
		{"interrupted data", testNetworkMessage(unix.RTM_NEWLINK, unix.NLM_F_DUMP_INTR, 41, 97, nil), 0},
		{"interrupted done", testNetworkMessage(unix.NLMSG_DONE, unix.NLM_F_DUMP_INTR, 41, 97, testNetworkStatus(0)), 0},
		{"short ack", testNetworkMessage(unix.NLMSG_ERROR, 0, 41, 97, testNetworkStatus(0)), 0},
		{"ack echo sequence", badEcho, 0},
		{"ack echo type", badEchoType, 0},
		{"ack echo port", badEchoPort, 0},
		{"ack echo length", badEchoLength, 0},
		{"ack echo flags", badEchoFlags, 0},
		{"positive errno", testNetworkACK(1), 0},
		{"invalid negative errno", testNetworkACK(-4096), 0},
		{"short done", testNetworkMessage(unix.NLMSG_DONE, 0, 41, 97, nil), 0},
		{"trailing bytes", append(done, 0), 0},
		{"data after done", append(append([]byte(nil), done...), data()...), 0},
		{"data after kernel error", append(testNetworkACK(-int32(unix.ENOENT)), data()...), 0},
		{"short alignment", data()[:18], 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testNetworkResponse()
			s.dump = true
			if done, err := s.consume(tt.packet, tt.flags); done || err == nil {
				t.Fatalf("accepted invalid reply: done=%v, err=%v", done, err)
			}
		})
	}
}

func TestNetworkKernelErrorsPreserveErrno(t *testing.T) {
	for _, packet := range [][]byte{
		testNetworkACK(-int32(unix.ENOENT)),
		testNetworkMessage(unix.NLMSG_DONE, unix.NLM_F_MULTI, 41, 97, testNetworkStatus(-int32(unix.ENOBUFS))),
	} {
		s := testNetworkResponse()
		s.dump = true
		done, err := s.consume(packet, 0)
		if done || (!errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOBUFS)) {
			t.Fatalf("kernel errno lost: done=%v, err=%v", done, err)
		}
		var terminal *networkKernelError
		if !errors.As(err, &terminal) {
			t.Fatalf("complete kernel error was not terminal: %v", err)
		}
	}
}

func TestNetworkDumpSizeIsBounded(t *testing.T) {
	s := testNetworkResponse()
	s.dump = true
	s.bytes = networkReplyLimit
	if done, err := s.consume(testNetworkMessage(unix.RTM_NEWLINK, unix.NLM_F_MULTI, 41, 97, nil), 0); done || err == nil {
		t.Fatalf("unbounded response accepted: done=%v, err=%v", done, err)
	}
}

func TestNetworkAttrsRejectContradictionsAndTruncation(t *testing.T) {
	attr := func(kind uint16, value []byte) []byte {
		b := make([]byte, nlAlign(4+len(value)))
		binary.NativeEndian.PutUint16(b, uint16(4+len(value)))
		binary.NativeEndian.PutUint16(b[2:], kind)
		copy(b[4:], value)
		return b
	}
	first := attr(1, []byte{3})
	second := attr(0x8002, []byte{4, 5})
	attrs, err := parseNetworkAttrs(append(append([]byte(nil), first...), second[:6]...))
	if err != nil || !bytes.Equal(attrs[1], []byte{3}) || !bytes.Equal(attrs[2], []byte{4, 5}) {
		t.Fatalf("valid attributes, including final unpadded attribute: %v, %v", attrs, err)
	}
	// Padding attribute numbers differ between netlink families. Keep empty
	// fields present so required-field length checks can still reject them.
	padding := append(attr(14, nil), attr(14, nil)...)
	attrs, err = parseNetworkAttrs(padding)
	if _, present := attrs[14]; err != nil || !present {
		t.Fatalf("empty padding was rejected or hidden: %v, %v", attrs, err)
	}
	for name, data := range map[string][]byte{
		"duplicate type":      append(append([]byte(nil), first...), first...),
		"empty then value":    append(attr(1, nil), first...),
		"short header":        {1, 0, 1},
		"short value":         first[:4],
		"short alignment":     first[:6],
		"length below header": {3, 0, 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if attrs, err := parseNetworkAttrs(data); err == nil || attrs != nil {
				t.Fatalf("accepted invalid attributes: %v, %v", attrs, err)
			}
		})
	}
}

func TestNetworkPollRespectsDeadlineAndCancellation(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := waitNetworkFD(ctx, fds[0], unix.POLLIN); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline not propagated: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("deadline took %v", elapsed)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		timer := time.AfterFunc(10*time.Millisecond, cancel)
		defer timer.Stop()
		start := time.Now()
		if err := waitNetworkFD(ctx, fds[0], unix.POLLIN); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not propagated: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("cancellation waited for the original deadline: %v", elapsed)
		}
	})
}

func openTestNetworkConn(t *testing.T) *networkConn {
	t.Helper()
	c, err := openNetworkConn(unix.NETLINK_ROUTE)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT) {
		t.Skipf("netlink unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	return c
}

// These read-only exchanges exercise actual kernel ACK and multipart ordering;
// they do not require CAP_NET_ADMIN or change the test host's network.
func TestNetworkReadOnlyKernelRoundTrips(t *testing.T) {
	c := openTestNetworkConn(t)
	flags, err := unix.FcntlInt(uintptr(c.fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK == 0 {
		t.Fatalf("netlink socket may block: flags=%#x, err=%v", flags, err)
	}
	fdFlags, err := unix.FcntlInt(uintptr(c.fd), unix.F_GETFD, 0)
	if err != nil || fdFlags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("netlink socket survives exec: flags=%#x, err=%v", fdFlags, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload := make([]byte, unix.SizeofIfInfomsg)
	for i := 0; i < 2; i++ {
		replies, err := c.request(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, payload)
		if err != nil || len(replies) == 0 {
			t.Fatalf("link dump %d: replies=%d, err=%v", i, len(replies), err)
		}
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	binary.NativeEndian.PutUint32(payload[4:], uint32(lo.Index))
	for i := 0; i < 2; i++ {
		replies, err := c.request(ctx, unix.RTM_GETLINK, unix.NLM_F_ACK, payload)
		if err != nil || len(replies) != 1 || replies[0].Type != unix.RTM_NEWLINK {
			t.Fatalf("query with ACK %d left an incomplete reply: %+v, %v", i, replies, err)
		}
	}
	// A socket disappearing between enumeration and an exact operation is a
	// complete error response, not a reason to discard the remaining work.
	binary.NativeEndian.PutUint32(payload[4:], 0x7fffffff)
	if _, err := c.request(ctx, unix.RTM_GETLINK, unix.NLM_F_ACK, payload); !errors.Is(err, unix.ENODEV) {
		t.Fatalf("invalid interface did not preserve ENODEV: %v", err)
	}
	if c.broken {
		t.Fatal("complete kernel errno retired connection")
	}
	binary.NativeEndian.PutUint32(payload[4:], uint32(lo.Index))
	if _, err := c.request(ctx, unix.RTM_GETLINK, unix.NLM_F_ACK, payload); err != nil {
		t.Fatalf("query after kernel errno: %v", err)
	}
}

func TestNetworkTimeoutRetiresConnection(t *testing.T) {
	c := openTestNetworkConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// A control NOOP without ACK has no reply. It allows exercising the real
	// send/receive timeout path without a privileged or destructive request.
	if _, err := c.request(ctx, unix.NLMSG_NOOP, 0, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request timeout: %v", err)
	}
	if !c.broken {
		t.Fatal("incomplete request left connection reusable")
	}
	if _, err := c.request(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, make([]byte, unix.SizeofIfInfomsg)); err == nil {
		t.Fatal("reused a connection with possible stale replies")
	}
}

func TestNetworkCloseDoesNotCloseReusedFD(t *testing.T) {
	source, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(source)
	owned, err := unix.FcntlInt(uintptr(source), unix.F_DUPFD_CLOEXEC, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	c := &networkConn{fd: owned}
	c.close()
	if err := unix.Dup3(source, owned, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(owned)
	c.close()
	if _, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0); err != nil {
		t.Fatalf("second close closed a reused fd: %v", err)
	}
	if _, err := c.request(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed connection accepted request: %v", err)
	}
}
