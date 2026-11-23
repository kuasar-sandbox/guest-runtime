package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type tcpRequesterFunc func(context.Context, uint16, uint16, []byte) ([]networkReply, error)

func (f tcpRequesterFunc) request(ctx context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
	return f(ctx, kind, flags, body)
}

func tcpTestDiag(family uint8, source, destination string, state uint8, cookie uint64) networkReply {
	body := make([]byte, 72)
	body[0], body[1] = family, state
	binary.BigEndian.PutUint16(body[4:6], 12345)
	binary.BigEndian.PutUint16(body[6:8], 443)
	src, dst := net.ParseIP(source), net.ParseIP(destination)
	if family == unix.AF_INET {
		copy(body[8:12], src.To4())
		copy(body[24:28], dst.To4())
	} else {
		copy(body[8:24], src.To16())
		copy(body[24:40], dst.To16())
	}
	binary.NativeEndian.PutUint64(body[44:52], cookie)
	binary.NativeEndian.PutUint32(body[64:68], 1000)
	// Leave inode == 0: full kernel-owned sockets must remain eligible.
	return tcpTestDiagAttrs(networkReply{Type: platformSockDiagByFamily, Data: body}, true, true, true)
}

func tcpTestDiagAttrs(reply networkReply, tos, tclass, mark bool) networkReply {
	body := append([]byte(nil), reply.Data[:72]...)
	if tos {
		body = nlAttr(body, platformDiagTOS, []byte{0x28})
	}
	if tclass && body[0] == unix.AF_INET6 {
		body = nlAttr(body, platformDiagTClass, []byte{0x60})
	}
	if mark {
		value := make([]byte, 4)
		binary.NativeEndian.PutUint32(value, 0x12345678)
		body = nlAttr(body, platformDiagMark, value)
	}
	reply.Data = body
	return reply
}

func tcpTestDump(t *testing.T, records ...networkReply) networkRequester {
	t.Helper()
	return tcpRequesterFunc(func(_ context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
		t.Helper()
		if kind != platformSockDiagByFamily || flags != unix.NLM_F_DUMP || len(body) != 56 {
			t.Fatalf("unexpected diagnostic request: kind=%d flags=%#x len=%d", kind, flags, len(body))
		}
		if body[1] != unix.IPPROTO_TCP || binary.NativeEndian.Uint32(body[4:8]) != 0x0b3e ||
			body[2] != 1<<(platformDiagTOS-1)|1<<(platformDiagTClass-1) {
			t.Fatalf("incorrect diagnostic protocol/states/extensions: %x", body[:8])
		}
		if binary.NativeEndian.Uint64(body[48:56]) != ^uint64(0) {
			t.Fatal("dump did not request the wildcard cookie")
		}
		var replies []networkReply
		for _, record := range records {
			if record.Data[0] == body[0] {
				replies = append(replies, record)
			}
		}
		return replies, nil
	})
}

func tcpTestRoute(family uint8, oif uint32, kind uint8) networkReply {
	body := make([]byte, 12)
	body[0], body[7] = family, kind
	value := make([]byte, 4)
	binary.NativeEndian.PutUint32(value, oif)
	body = nlAttr(body, unix.RTA_OIF, value)
	return networkReply{Type: unix.RTM_NEWROUTE, Data: body}
}

func tcpTestNoRoute(t *testing.T) networkRequester {
	t.Helper()
	return tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
		t.Fatal("ineligible socket triggered a route request")
		return nil, nil
	})
}

func TestCollectPlatformTCPUsesActualRoute(t *testing.T) {
	const platform = 7
	records := []networkReply{
		tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1),
		tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.11", 1, 2),
		tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.12", 1, 3),
		tcpTestDiag(unix.AF_INET, "192.0.2.3", "198.51.100.13", 1, 4),
		tcpTestDiag(unix.AF_INET, "192.0.2.2", "192.0.2.2", 1, 5),
		tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.14", 1, 6),
	}
	binary.NativeEndian.PutUint32(records[2].Data[40:44], 8)
	queries := 0
	route := tcpRequesterFunc(func(_ context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
		queries++
		if kind != unix.RTM_GETROUTE || flags != 0 || body[0] != unix.AF_INET || body[1] != 32 || body[2] != 32 || body[3] != 0x28 ||
			binary.NativeEndian.Uint32(body[8:12]) != 0x3000 {
			t.Fatalf("incorrect IPv4 FIB request: kind=%d flags=%#x body=%x", kind, flags, body)
		}
		attrs, err := parseNetworkAttrs(body[12:])
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := attrs[unix.RTA_OIF]; exists {
			t.Fatal("unbound socket lookup forced an output interface")
		}
		if !bytes.Equal(attrs[unix.RTA_SRC], net.ParseIP("192.0.2.2").To4()) ||
			!bytes.Equal(attrs[unix.RTA_SPORT], []byte{0x30, 0x39}) ||
			!bytes.Equal(attrs[unix.RTA_DPORT], []byte{0x01, 0xbb}) ||
			!bytes.Equal(attrs[unix.RTA_IP_PROTO], []byte{unix.IPPROTO_TCP}) ||
			binary.NativeEndian.Uint32(attrs[unix.RTA_UID]) != 1000 ||
			binary.NativeEndian.Uint32(attrs[unix.RTA_MARK]) != 0x12345678 {
			t.Fatalf("lost route identity: %#v", attrs)
		}
		switch net.IP(attrs[unix.RTA_DST]).String() {
		case "198.51.100.10":
			return []networkReply{tcpTestRoute(unix.AF_INET, platform, unix.RTN_UNICAST)}, nil
		case "198.51.100.11":
			return []networkReply{tcpTestRoute(unix.AF_INET, 8, unix.RTN_UNICAST)}, nil
		case "192.0.2.2":
			return []networkReply{tcpTestRoute(unix.AF_INET, platform, unix.RTN_LOCAL)}, nil
		case "198.51.100.14":
			return nil, fmt.Errorf("route miss: %w", unix.ENETUNREACH)
		default:
			t.Fatalf("bound-other or foreign-source socket was looked up: %#v", attrs)
			return nil, nil
		}
	})
	candidates, err := collectPlatformTCP(context.Background(), route, tcpTestDump(t, records...), platform, []net.IP{net.ParseIP("192.0.2.2")})
	if err != nil || len(candidates) != 1 || queries != 4 {
		t.Fatalf("candidates=%v queries=%d err=%v", candidates, queries, err)
	}
	if binary.NativeEndian.Uint64(candidates[0].id[40:48]) != 1 {
		t.Fatal("selected a user-interface or local socket")
	}
}

func TestCollectPlatformTCPIPv6AndMappedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      string
		destination string
		oldAddress  string
		routeFamily uint8
		tos         uint8
		bound       uint32
	}{
		{"native", "2001:db8::2", "2001:db8:1::3", "2001:db8::2", unix.AF_INET6, 0x60, 7},
		{"mapped", "::ffff:192.0.2.2", "::ffff:198.51.100.10", "192.0.2.2", unix.AF_INET, 0x28, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := tcpTestDiag(unix.AF_INET6, tc.source, tc.destination, 1, 0x123456789abcdef0)
			binary.NativeEndian.PutUint32(record.Data[40:44], tc.bound)
			route := tcpRequesterFunc(func(_ context.Context, _ uint16, _ uint16, body []byte) ([]networkReply, error) {
				prefix, fibFlags := uint8(128), uint32(0x2000)
				source, destination := net.ParseIP(tc.source).To16(), net.ParseIP(tc.destination).To16()
				if tc.routeFamily == unix.AF_INET {
					prefix, fibFlags = 32, 0x3000
					source, destination = source.To4(), destination.To4()
				}
				if body[0] != tc.routeFamily || body[1] != prefix || body[2] != prefix || body[3] != tc.tos ||
					binary.NativeEndian.Uint32(body[8:12]) != fibFlags {
					t.Fatalf("incorrect IPv6/mapped FIB header: %x", body[:12])
				}
				attrs, err := parseNetworkAttrs(body[12:])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(attrs[unix.RTA_SRC], source) || !bytes.Equal(attrs[unix.RTA_DST], destination) ||
					!bytes.Equal(attrs[unix.RTA_SPORT], record.Data[4:6]) || !bytes.Equal(attrs[unix.RTA_DPORT], record.Data[6:8]) {
					t.Fatalf("incorrect IPv6/mapped route identity: %#v", attrs)
				}
				oif, exists := attrs[unix.RTA_OIF]
				if exists != (tc.bound != 0) || (exists && binary.NativeEndian.Uint32(oif) != tc.bound) {
					t.Fatalf("incorrect bound interface: %x", oif)
				}
				return []networkReply{tcpTestRoute(tc.routeFamily, 7, unix.RTN_UNICAST)}, nil
			})
			candidates, err := collectPlatformTCP(context.Background(), route, tcpTestDump(t, record), 7, []net.IP{net.ParseIP(tc.oldAddress)})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%v err=%v", candidates, err)
			}
			calls := 0
			destroy := tcpRequesterFunc(func(_ context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
				calls++
				if kind != platformSockDestroy || flags != unix.NLM_F_ACK || len(body) != 56 || body[0] != unix.AF_INET6 || body[1] != unix.IPPROTO_TCP ||
					binary.NativeEndian.Uint32(body[4:8]) != ^uint32(0) || !bytes.Equal(body[8:56], record.Data[4:52]) {
					t.Fatalf("destroy changed original family, tuple, bound interface or cookie: %x", body)
				}
				return nil, nil
			})
			result := releasePlatformTCP(context.Background(), destroy, candidates)
			if result.Selected != 1 || result.Released != 1 || result.Err != nil || calls != 1 {
				t.Fatalf("release=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestCollectPlatformTCPSkipsMissingMetadataAndNonFullSockets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(networkReply) networkReply
	}{
		{"missing TOS", func(r networkReply) networkReply { return tcpTestDiagAttrs(r, false, true, true) }},
		{"missing mark", func(r networkReply) networkReply { return tcpTestDiagAttrs(r, true, true, false) }},
		{"request socket", func(r networkReply) networkReply { r.Data = r.Data[:72]; r.Data[1] = 3; return r }},
		{"listener", func(r networkReply) networkReply { r.Data[1] = 10; return r }},
		{"time wait", func(r networkReply) networkReply { r.Data[1] = 6; return r }},
		{"closed", func(r networkReply) networkReply { r.Data[1] = 7; return r }},
		{"no cookie", func(r networkReply) networkReply {
			binary.NativeEndian.PutUint64(r.Data[44:52], ^uint64(0))
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := tc.mutate(tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1))
			candidates, err := collectPlatformTCP(context.Background(), tcpTestNoRoute(t), tcpTestDump(t, record), 7, []net.IP{net.ParseIP("192.0.2.2")})
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates=%v err=%v", candidates, err)
			}
		})
	}
	for _, source := range []string{"2001:db8::2", "::ffff:192.0.2.2"} {
		record := tcpTestDiag(unix.AF_INET6, source, "2001:db8:1::3", 1, 1)
		if source == "::ffff:192.0.2.2" {
			record = tcpTestDiag(unix.AF_INET6, source, "::ffff:198.51.100.10", 1, 1)
		}
		record = tcpTestDiagAttrs(record, true, false, true)
		candidates, err := collectPlatformTCP(context.Background(), tcpTestNoRoute(t), tcpTestDump(t, record), 7, []net.IP{net.ParseIP(source)})
		if err != nil || len(candidates) != 0 {
			t.Fatalf("missing TCLASS: candidates=%v err=%v", candidates, err)
		}
	}
}

func TestCollectPlatformTCPSkipsComplexRouteShapes(t *testing.T) {
	for _, kind := range []uint16{unix.RTA_MULTIPATH, platformRouteNHID, unix.RTA_ENCAP, unix.RTA_ENCAP_TYPE, unix.RTA_NEWDST, unix.RTA_VIA} {
		t.Run(fmt.Sprintf("attribute_%d", kind), func(t *testing.T) {
			record := tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1)
			route := tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
				reply := tcpTestRoute(unix.AF_INET, 7, unix.RTN_UNICAST)
				reply.Data = nlAttr(reply.Data, kind, []byte{0, 0, 0, 0})
				return []networkReply{reply}, nil
			})
			candidates, err := collectPlatformTCP(context.Background(), route, tcpTestDump(t, record), 7, []net.IP{net.ParseIP("192.0.2.2")})
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates=%v err=%v", candidates, err)
			}
		})
	}
}

func TestCollectPlatformTCPRejectsIncompleteBatch(t *testing.T) {
	for _, failure := range []string{"second dump", "bad metadata length", "bad route length", "multiple routes", "wrong route family"} {
		t.Run(failure, func(t *testing.T) {
			records := []networkReply{
				tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1),
				tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.11", 1, 2),
			}
			if failure == "bad metadata length" {
				records[1] = tcpTestDiagAttrs(records[1], false, true, true)
				records[1].Data = nlAttr(records[1].Data, platformDiagTOS, []byte{0, 0})
			}
			baseDump := tcpTestDump(t, records...)
			diag := tcpRequesterFunc(func(ctx context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
				if failure == "second dump" && body[0] == unix.AF_INET6 {
					return nil, unix.EINTR
				}
				return baseDump.request(ctx, kind, flags, body)
			})
			queries := 0
			route := tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
				queries++
				reply := tcpTestRoute(unix.AF_INET, 7, unix.RTN_UNICAST)
				if queries == 2 {
					switch failure {
					case "bad route length":
						reply.Data = reply.Data[:11]
					case "multiple routes":
						return []networkReply{reply, reply}, nil
					case "wrong route family":
						reply.Data[0] = unix.AF_INET6
					}
				}
				return []networkReply{reply}, nil
			})
			candidates, err := collectPlatformTCP(context.Background(), route, diag, 7, []net.IP{net.ParseIP("192.0.2.2")})
			if err == nil || candidates != nil {
				t.Fatalf("released an incomplete collection: candidates=%v err=%v", candidates, err)
			}
		})
	}
}

func TestCollectPlatformTCPIPv4OnlyKernel(t *testing.T) {
	record := tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1)
	baseDump := tcpTestDump(t, record)
	diag := tcpRequesterFunc(func(ctx context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
		if body[0] == unix.AF_INET6 {
			return nil, unix.EAFNOSUPPORT
		}
		return baseDump.request(ctx, kind, flags, body)
	})
	route := tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
		return []networkReply{tcpTestRoute(unix.AF_INET, 7, unix.RTN_UNICAST)}, nil
	})
	candidates, err := collectPlatformTCP(context.Background(), route, diag, 7, []net.IP{net.ParseIP("192.0.2.2")})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("IPv4-only kernel: candidates=%v err=%v", candidates, err)
	}
	candidates, err = collectPlatformTCP(context.Background(), route, diag, 7, []net.IP{net.ParseIP("192.0.2.2"), net.ParseIP("2001:db8::2")})
	if !errors.Is(err, unix.EAFNOSUPPORT) || candidates != nil {
		t.Fatalf("ignored missing IPv6 diagnostics with an IPv6 source: candidates=%v err=%v", candidates, err)
	}
}

func TestCollectPlatformTCPCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	diag := tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
		cancel()
		return []networkReply{tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, 1)}, nil
	})
	candidates, err := collectPlatformTCP(ctx, tcpTestNoRoute(t), diag, 7, []net.IP{net.ParseIP("192.0.2.2")})
	if !errors.Is(err, context.Canceled) || candidates != nil {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
}

func tcpTestCandidates(t *testing.T, count int) []platformTCP {
	t.Helper()
	candidates := make([]platformTCP, count)
	for i := range candidates {
		record := tcpTestDiag(unix.AF_INET, "192.0.2.2", "198.51.100.10", 1, uint64(i+1))
		candidate, ok, err := decodePlatformTCP(record, unix.AF_INET)
		if err != nil || !ok {
			t.Fatalf("test candidate failed to decode: %v", err)
		}
		candidates[i] = candidate
	}
	return candidates
}

func TestReleasePlatformTCPFixedBatchContinuesAfterSingleFailure(t *testing.T) {
	candidates := tcpTestCandidates(t, 4)
	errorsByIndex := []error{nil, unix.ENOENT, unix.EIO, nil}
	calls := 0
	diag := tcpRequesterFunc(func(_ context.Context, kind, flags uint16, body []byte) ([]networkReply, error) {
		if calls >= len(candidates) {
			t.Fatal("release retried or rescanned")
		}
		if kind != platformSockDestroy || flags != unix.NLM_F_ACK || body[0] != unix.AF_INET ||
			!bytes.Equal(body[8:56], candidates[calls].id[:]) {
			t.Fatalf("destroy changed identity at index %d: %x", calls, body)
		}
		err := errorsByIndex[calls]
		calls++
		return nil, err
	})
	result := releasePlatformTCP(context.Background(), diag, candidates)
	if result.Selected != 4 || result.Released != 2 || result.Vanished != 1 || result.Failed != 1 || result.Skipped != 0 ||
		!errors.Is(result.Err, unix.EIO) || calls != 4 {
		t.Fatalf("release=%+v calls=%d", result, calls)
	}
}

func TestReleasePlatformTCPGlobalFailureStopsBatch(t *testing.T) {
	for _, requestErr := range []error{unix.EPERM, unix.EACCES, unix.EOPNOTSUPP, unix.EAFNOSUPPORT} {
		t.Run(requestErr.Error(), func(t *testing.T) {
			calls := 0
			diag := tcpRequesterFunc(func(context.Context, uint16, uint16, []byte) ([]networkReply, error) {
				calls++
				return nil, requestErr
			})
			result := releasePlatformTCP(context.Background(), diag, tcpTestCandidates(t, 4))
			if result.Selected != 4 || result.Failed != 1 || result.Skipped != 3 || result.Released != 0 ||
				!errors.Is(result.Err, requestErr) || calls != 1 {
				t.Fatalf("release=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestReleasePlatformTCPContextBoundsWork(t *testing.T) {
	for _, canceledBefore := range []bool{true, false} {
		t.Run(fmt.Sprintf("canceled_before_%v", canceledBefore), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if canceledBefore {
				cancel()
			}
			calls := 0
			diag := tcpRequesterFunc(func(ctx context.Context, _ uint16, _ uint16, _ []byte) ([]networkReply, error) {
				calls++
				<-ctx.Done()
				return nil, ctx.Err()
			})
			start := time.Now()
			result := releasePlatformTCP(ctx, diag, tcpTestCandidates(t, 4))
			if time.Since(start) > time.Second {
				t.Fatal("release exceeded the context deadline by more than one second")
			}
			wantCalls, wantSkipped := 1, 3
			wantErr := context.DeadlineExceeded
			if canceledBefore {
				wantCalls, wantSkipped, wantErr = 0, 4, context.Canceled
			}
			if result.Selected != 4 || result.Failed != wantCalls || result.Skipped != wantSkipped ||
				result.Released != 0 || !errors.Is(result.Err, wantErr) || calls != wantCalls {
				t.Fatalf("release=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestReleasePlatformTCPRejectsWildcardCookie(t *testing.T) {
	candidates := tcpTestCandidates(t, 2)
	binary.NativeEndian.PutUint64(candidates[0].id[40:48], ^uint64(0))
	calls := 0
	diag := tcpRequesterFunc(func(_ context.Context, _ uint16, _ uint16, body []byte) ([]networkReply, error) {
		calls++
		if !bytes.Equal(body[8:56], candidates[1].id[:]) {
			t.Fatal("sent wildcard-cookie destroy")
		}
		return nil, nil
	})
	result := releasePlatformTCP(context.Background(), diag, candidates)
	if result.Selected != 2 || result.Failed != 1 || result.Released != 1 || result.Skipped != 0 || result.Err == nil || calls != 1 {
		t.Fatalf("release=%+v calls=%d", result, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := releasePlatformTCP(ctx, diag, nil); result != (tcpCleanupResult{}) {
		t.Fatalf("empty batch is not a no-op: %+v", result)
	}
}
