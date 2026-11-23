package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

const (
	platformSockDiagByFamily = 20
	platformSockDestroy      = 21
	platformDiagTOS          = 5
	platformDiagTClass       = 6
	platformDiagMark         = 15
	platformTCPStates        = 0x0b3e
	platformRouteLookupTable = 0x1000
	platformRouteFIBMatch    = 0x2000
	// RTA_NH_ID from linux/rtnetlink.h is not exported by x/sys/unix.
	platformRouteNHID = 30
)

// Keep the original family and complete sockid, including its cookie. A mapped
// IPv6 socket uses an IPv4 route lookup but still needs an IPv6 destroy request.
type platformTCP struct {
	family      uint8
	id          [48]byte
	routeFamily uint8
	source      netip.Addr
	destination netip.Addr
	uid         uint32
	mark        uint32
	tos         uint8
}

// collectPlatformTCP runs in the original management netns, before changing its
// addresses or routes. It deliberately covers the sandboxer's ordinary L3
// network, not arbitrary user-created network namespaces or routing machinery.
// An incomplete dump or lookup aborts collection: never release a partial batch.
func collectPlatformTCP(ctx context.Context, route networkRequester, diag networkRequester, ifindex int32, oldAddrs []net.IP) ([]platformTCP, error) {
	if ifindex <= 0 {
		return nil, fmt.Errorf("invalid platform interface index %d", ifindex)
	}
	addresses := make(map[netip.Addr]struct{}, len(oldAddrs))
	hasIPv4, hasIPv6 := false, false
	for _, ip := range oldAddrs {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		addresses[addr] = struct{}{}
		if addr.Is4() {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}
	}
	if len(addresses) == 0 {
		return nil, nil
	}
	families := []uint8{unix.AF_INET6}
	if hasIPv4 {
		// IPv4-mapped sockets appear in the IPv6 diagnostic dump.
		families = []uint8{unix.AF_INET, unix.AF_INET6}
	}
	var candidates []platformTCP
	for _, family := range families {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req := make([]byte, 56) // struct inet_diag_req_v2
		req[0], req[1] = family, unix.IPPROTO_TCP
		req[2] = 1<<(platformDiagTOS-1) | 1<<(platformDiagTClass-1)
		binary.NativeEndian.PutUint32(req[4:8], platformTCPStates)
		binary.NativeEndian.PutUint64(req[48:56], ^uint64(0))
		replies, err := diag.request(ctx, platformSockDiagByFamily, unix.NLM_F_DUMP, req)
		if err != nil {
			if family == unix.AF_INET6 && !hasIPv6 &&
				(errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT)) {
				// An IPv4-only kernel cannot contain mapped IPv6 sockets.
				continue
			}
			return nil, fmt.Errorf("dump TCP family %d: %w", family, err)
		}
		for _, reply := range replies {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			candidate, usable, err := decodePlatformTCP(reply, family)
			if err != nil {
				return nil, err
			}
			if !usable {
				continue
			}
			if _, exists := addresses[candidate.source]; !exists {
				continue
			}
			bound := binary.NativeEndian.Uint32(candidate.id[36:40])
			if bound != 0 && bound != uint32(ifindex) {
				continue
			}
			matches, err := platformTCPUsesInterface(ctx, route, candidate, ifindex)
			if err != nil {
				return nil, fmt.Errorf("classify TCP route: %w", err)
			}
			if matches {
				candidates = append(candidates, candidate)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func decodePlatformTCP(reply networkReply, family uint8) (platformTCP, bool, error) {
	var candidate platformTCP
	if reply.Type != platformSockDiagByFamily || len(reply.Data) < 72 {
		return candidate, false, fmt.Errorf("invalid TCP diagnostic message")
	}
	body := reply.Data
	if body[0] != family {
		return candidate, false, fmt.Errorf("TCP diagnostic family %d, expected %d", body[0], family)
	}
	if body[1] >= 32 || platformTCPStates&(uint32(1)<<body[1]) == 0 {
		return candidate, false, nil // Includes listeners and TIME_WAIT records.
	}
	candidate.family = family
	copy(candidate.id[:], body[4:52])
	if !platformTCPCookieValid(candidate.id) {
		return candidate, false, nil
	}
	var ok bool
	if family == unix.AF_INET {
		candidate.source, ok = netip.AddrFromSlice(candidate.id[4:8])
		if !ok {
			return candidate, false, fmt.Errorf("invalid diagnostic IPv4 source")
		}
		candidate.destination, _ = netip.AddrFromSlice(candidate.id[20:24])
		candidate.routeFamily = unix.AF_INET
	} else if family == unix.AF_INET6 {
		candidate.source, _ = netip.AddrFromSlice(candidate.id[4:20])
		candidate.destination, _ = netip.AddrFromSlice(candidate.id[20:36])
		if candidate.source.Is4In6() != candidate.destination.Is4In6() {
			return candidate, false, nil
		}
		candidate.routeFamily = unix.AF_INET6
		if candidate.source.Is4In6() {
			candidate.source = candidate.source.Unmap()
			candidate.destination = candidate.destination.Unmap()
			candidate.routeFamily = unix.AF_INET
		}
	} else {
		return candidate, false, fmt.Errorf("unsupported diagnostic family %d", family)
	}
	attrs, err := parseNetworkAttrs(body[72:])
	if err != nil {
		return candidate, false, fmt.Errorf("parse TCP diagnostic attributes: %w", err)
	}
	for kind, size := range map[uint16]int{platformDiagTOS: 1, platformDiagTClass: 1, platformDiagMark: 4} {
		if attr, exists := attrs[kind]; exists && len(attr) != size {
			return candidate, false, fmt.Errorf("invalid TCP diagnostic attribute %d length %d", kind, len(attr))
		}
	}
	tos, hasTOS := attrs[platformDiagTOS]
	tclass, hasTClass := attrs[platformDiagTClass]
	mark, hasMark := attrs[platformDiagMark]
	if !hasTOS || !hasMark || (family == unix.AF_INET6 && !hasTClass) {
		// Request sockets and unprivileged dumps may omit metadata. Missing is
		// different from an actual zero mark or traffic class.
		return candidate, false, nil
	}
	candidate.tos = tos[0]
	if candidate.routeFamily == unix.AF_INET6 {
		candidate.tos = tclass[0]
	}
	candidate.uid = binary.NativeEndian.Uint32(body[64:68])
	candidate.mark = binary.NativeEndian.Uint32(mark)
	// inode == 0 is valid for kernel-owned full sockets, including NFS.
	return candidate, true, nil
}

func platformTCPCookieValid(id [48]byte) bool {
	return binary.NativeEndian.Uint64(id[40:48]) != ^uint64(0)
}

func platformTCPUsesInterface(ctx context.Context, route networkRequester, candidate platformTCP, ifindex int32) (bool, error) {
	req := make([]byte, 12) // struct rtmsg
	req[0], req[3] = candidate.routeFamily, candidate.tos
	prefix := uint8(128)
	flags := uint32(platformRouteFIBMatch)
	if candidate.routeFamily == unix.AF_INET {
		prefix = 32
		flags |= platformRouteLookupTable
	}
	req[1], req[2] = prefix, prefix
	binary.NativeEndian.PutUint32(req[8:12], flags)
	req = nlAttr(req, unix.RTA_SRC, candidate.source.AsSlice())
	req = nlAttr(req, unix.RTA_DST, candidate.destination.AsSlice())
	req = nlAttr(req, unix.RTA_IP_PROTO, []byte{unix.IPPROTO_TCP})
	// inet_diag_sockid and these route attributes both store ports in network
	// byte order. Copy them without a native-endian round trip.
	req = nlAttr(req, unix.RTA_SPORT, candidate.id[0:2])
	req = nlAttr(req, unix.RTA_DPORT, candidate.id[2:4])
	u32 := make([]byte, 4)
	binary.NativeEndian.PutUint32(u32, candidate.uid)
	req = nlAttr(req, unix.RTA_UID, u32)
	binary.NativeEndian.PutUint32(u32, candidate.mark)
	req = nlAttr(req, unix.RTA_MARK, u32)
	if bound := binary.NativeEndian.Uint32(candidate.id[36:40]); bound != 0 {
		// An unbound socket has no OIF constraint. Forcing P here would turn a
		// user-interface route into an incorrect match for the platform.
		req = nlAttr(req, unix.RTA_OIF, candidate.id[36:40])
	}
	replies, err := route.request(ctx, unix.RTM_GETROUTE, 0, req)
	if err != nil {
		if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) ||
			errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES) {
			return false, nil // No usable route (including a prohibit route).
		}
		return false, err
	}
	if len(replies) != 1 || replies[0].Type != unix.RTM_NEWROUTE || len(replies[0].Data) < 12 {
		return false, fmt.Errorf("invalid TCP route reply")
	}
	body := replies[0].Data
	if body[0] != candidate.routeFamily {
		return false, fmt.Errorf("TCP route reply family mismatch")
	}
	attrs, err := parseNetworkAttrs(body[12:])
	if err != nil {
		return false, fmt.Errorf("parse TCP route attributes: %w", err)
	}
	if body[7] != unix.RTN_UNICAST {
		return false, nil
	}
	for _, kind := range []uint16{unix.RTA_MULTIPATH, platformRouteNHID, unix.RTA_ENCAP, unix.RTA_ENCAP_TYPE, unix.RTA_NEWDST, unix.RTA_VIA} {
		if _, exists := attrs[kind]; exists {
			return false, nil
		}
	}
	oif, exists := attrs[unix.RTA_OIF]
	if !exists {
		return false, nil
	}
	if len(oif) != 4 {
		return false, fmt.Errorf("invalid TCP route OIF length %d", len(oif))
	}
	return binary.NativeEndian.Uint32(oif) == uint32(ifindex), nil
}

type tcpCleanupResult struct {
	Selected, Released, Vanished, Failed, Skipped int
	Err                                           error
}

// releasePlatformTCP only destroys the fixed original cookies. It never retries,
// rescans or waits for a peer. Failed includes requests without a successful
// ACK; after an ACK timeout the kernel may already have destroyed that socket.
// Only the first error is kept, so a large failed batch cannot create a huge log.
func releasePlatformTCP(ctx context.Context, diag networkRequester, candidates []platformTCP) tcpCleanupResult {
	result := tcpCleanupResult{Selected: len(candidates)}
	setError := func(err error) {
		if result.Err == nil {
			result.Err = err
		}
	}
	for i, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Skipped = len(candidates) - i
			setError(err)
			break
		}
		if (candidate.family != unix.AF_INET && candidate.family != unix.AF_INET6) || !platformTCPCookieValid(candidate.id) {
			result.Failed++
			setError(fmt.Errorf("invalid TCP identity at batch index %d", i))
			continue
		}
		req := make([]byte, 56)
		req[0], req[1] = candidate.family, unix.IPPROTO_TCP
		binary.NativeEndian.PutUint32(req[4:8], ^uint32(0))
		copy(req[8:56], candidate.id[:])
		_, err := diag.request(ctx, platformSockDestroy, unix.NLM_F_ACK, req)
		switch {
		case err == nil:
			result.Released++
		case errors.Is(err, unix.ENOENT):
			result.Vanished++
		default:
			result.Failed++
			setError(fmt.Errorf("release TCP batch index %d: %w", i, err))
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EOPNOTSUPP) ||
				errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT) || errors.Is(err, unix.ENOSYS) {
				result.Skipped = len(candidates) - i - 1
				return result
			}
		}
	}
	return result
}
