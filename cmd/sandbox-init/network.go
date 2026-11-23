// Platform IP configuration and restore-local network state. All sockets are
// short-lived and remain in the outer init's original network namespace.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const (
	networkOperationBudget = time.Second
	platformTCPBudget      = 50 * time.Millisecond
	nlAlignTo              = 4
)

func nlAlign(n int) int { return (n + nlAlignTo - 1) &^ (nlAlignTo - 1) }

func nlAttr(buf []byte, attrType uint16, value []byte) []byte {
	a := make([]byte, nlAlign(4+len(value)))
	binary.NativeEndian.PutUint16(a, uint16(4+len(value)))
	binary.NativeEndian.PutUint16(a[2:], attrType)
	copy(a[4:], value)
	return append(buf, a...)
}

func networkUint32(n uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, n)
	return b
}

func addrBytes(family int, ip net.IP) []byte {
	if family == unix.AF_INET {
		return ip.To4()
	}
	return ip.To16()
}

func bringUpLoopback() error {
	index, err := readIfindex("lo")
	if err != nil {
		return err
	}
	c, err := openNetworkConn(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), networkOperationBudget)
	defer cancel()
	return networkLinkUp(ctx, c, index, 0)
}

type platformAddress struct {
	family, prefix, scope int
	ip                    net.IP
}

type platformConfig struct {
	family, prefix, mtu int
	ip, gateway         net.IP
}

func parsePlatformConfig(spec *proto.NetworkSpec) (*platformConfig, error) {
	if spec == nil || spec.IPCIDR == "" {
		return nil, nil
	}
	ip, ipnet, err := net.ParseCIDR(spec.IPCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cidr %q: %w", spec.IPCIDR, err)
	}
	prefix, bits := ipnet.Mask.Size()
	cfg := &platformConfig{family: unix.AF_INET, ip: ip, prefix: prefix, mtu: spec.MTU}
	if bits == 128 {
		if ip.To4() != nil {
			return nil, fmt.Errorf("mapped IPv6 is not an interface address: %q", spec.IPCIDR)
		}
		cfg.family = unix.AF_INET6
	}
	if spec.MTU < 0 {
		return nil, fmt.Errorf("negative MTU: %d", spec.MTU)
	}
	if spec.Nexthop != "" {
		cfg.gateway = net.ParseIP(spec.Nexthop)
		if cfg.gateway == nil || (cfg.gateway.To4() != nil) != (cfg.family == unix.AF_INET) {
			return nil, fmt.Errorf("nexthop %q must match the address family", spec.Nexthop)
		}
	}
	return cfg, nil
}

func applyNetwork(spec *proto.NetworkSpec) error {
	cfg, err := parsePlatformConfig(spec)
	if err != nil {
		return err
	}
	if err := applyNetworkHostname(spec); err != nil || cfg == nil {
		return err
	}
	c, err := openNetworkConn(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), networkOperationBudget)
	defer cancel()
	index, err := guestNetwork.resolve(ctx, c)
	if err != nil {
		return err
	}
	if index == 0 {
		return errors.New("platform interface is unavailable")
	}
	return guestNetwork.configure(ctx, c, index, cfg, nil, false)
}

func applyNetworkHostname(spec *proto.NetworkSpec) error {
	if spec != nil && spec.Hostname != "" {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
	}
	return nil
}

// applyNetworkReplace is called for every true Restore, including nil or
// unchanged NetworkSpec. Attach and cold boot never release old connections.
// TCP errors are diagnostics only; they cannot become a restore readiness gate.
func applyNetworkReplace(spec *proto.NetworkSpec) error {
	cfg, cfgErr := parsePlatformConfig(spec)
	if cfgErr == nil {
		cfgErr = applyNetworkHostname(spec)
	}
	c, err := openNetworkConn(unix.NETLINK_ROUTE)
	if err != nil {
		return errors.Join(cfgErr, err)
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), networkOperationBudget)
	defer cancel()
	index, err := guestNetwork.resolve(ctx, c)
	if err != nil || index == 0 {
		return errors.Join(cfgErr, err)
	}
	return resetPlatformNetwork(ctx, c, &guestNetwork, index, cfg, cfgErr, func(protocol int) (networkSession, error) {
		conn, err := openNetworkConn(protocol)
		if err != nil {
			return nil, err
		}
		return conn, nil
	})
}

type networkSession interface {
	networkRequester
	close()
}

// Separated from device binding so fault tests can exercise the same complete
// coordinator without mutating the test runner's network.
func resetPlatformNetwork(ctx context.Context, c networkRequester, p *platformNetwork, index int32, cfg *platformConfig, cfgErr error, openTCP func(int) (networkSession, error)) error {
	started := time.Now()
	old, addrErr := networkAddresses(ctx, c, index)
	var ips []net.IP
	for _, a := range old {
		ips = append(ips, a.ip)
	}
	// A separate route socket keeps a failed TCP lookup from retiring the
	// configuration socket. The fixed deadline is shared by dump, lookup and
	// exact destroys; it is never renewed for each connection.
	tcpStart := time.Now()
	tcpCtx, tcpCancel := context.WithTimeout(ctx, platformTCPBudget)
	defer tcpCancel()
	var candidates []platformTCP
	var diag networkSession
	tcpErr := addrErr
	if tcpErr == nil && len(ips) != 0 {
		var route networkSession
		route, tcpErr = openTCP(unix.NETLINK_ROUTE)
		if tcpErr == nil {
			diag, tcpErr = openTCP(unix.NETLINK_SOCK_DIAG)
			if tcpErr == nil {
				defer diag.close()
				candidates, tcpErr = collectPlatformTCP(tcpCtx, route, diag, index, ips)
			}
			route.close()
		}
	}
	tcpElapsed := time.Since(tcpStart)
	// Census and route attribution above use the old identity, before address
	// removal can invalidate the route lookup. Configuration errors retain the
	// existing best-effort Restore policy and remain separate from TCP results.
	if cfgErr == nil && cfg != nil {
		if addrErr != nil {
			cfgErr = addrErr
		} else {
			cfgErr = p.configure(ctx, c, index, cfg, old, true)
		}
	}
	neighborErr := flushPlatformNeighbors(ctx, c, index)
	result := tcpCleanupResult{Err: tcpErr}
	if tcpErr == nil && len(candidates) != 0 {
		releaseStarted := time.Now()
		result = releasePlatformTCP(tcpCtx, diag, candidates)
		tcpElapsed += time.Since(releaseStarted)
	}
	logf("network restore: elapsed=%s tcp_elapsed=%s selected=%d released=%d vanished=%d failed=%d skipped=%d tcp_error=%v", time.Since(started), tcpElapsed, result.Selected, result.Released, result.Vanished, result.Failed, result.Skipped, result.Err)
	return errors.Join(cfgErr, neighborErr)
}

func networkLinkUp(ctx context.Context, c networkRequester, index int32, mtu int) error {
	body := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(body[4:], uint32(index))
	binary.NativeEndian.PutUint32(body[8:], unix.IFF_UP)
	binary.NativeEndian.PutUint32(body[12:], unix.IFF_UP)
	if mtu > 0 {
		body = nlAttr(body, unix.IFLA_MTU, networkUint32(uint32(mtu)))
	}
	_, err := c.request(ctx, unix.RTM_NEWLINK, unix.NLM_F_ACK, body)
	return err
}

func networkAddresses(ctx context.Context, c networkRequester, index int32) ([]platformAddress, error) {
	body := make([]byte, unix.SizeofIfAddrmsg)
	replies, err := c.request(ctx, unix.RTM_GETADDR, unix.NLM_F_DUMP, body)
	if err != nil {
		return nil, err
	}
	var out []platformAddress
	for _, r := range replies {
		if r.Type != unix.RTM_NEWADDR || len(r.Data) < unix.SizeofIfAddrmsg {
			return nil, errors.New("invalid address dump reply")
		}
		p := r.Data
		family := int(p[0])
		if int32(binary.NativeEndian.Uint32(p[4:])) != index || family != unix.AF_INET && family != unix.AF_INET6 {
			continue
		}
		attrs, err := parseNetworkAttrs(p[unix.SizeofIfAddrmsg:])
		if err != nil {
			return nil, err
		}
		ip := attrs[unix.IFA_LOCAL]
		if ip == nil {
			ip = attrs[unix.IFA_ADDRESS]
		}
		want := 4
		if family == unix.AF_INET6 {
			want = 16
		}
		if len(ip) != want || int(p[1]) > 8*want {
			return nil, errors.New("invalid platform address")
		}
		out = append(out, platformAddress{family: family, prefix: int(p[1]), scope: int(p[3]), ip: net.IP(append([]byte(nil), ip...))})
	}
	return out, nil
}

// Only the exact successfully installed default is owned by the platform. A
// restore without a new IP keeps configuration; an empty gateway WITH a new
// IP removes the owned default. No route dump/blanket default-route flush.
func (p *platformNetwork) configure(ctx context.Context, c networkRequester, index int32, cfg *platformConfig, old []platformAddress, replace bool) error {
	if err := networkLinkUp(ctx, c, index, cfg.mtu); err != nil {
		return fmt.Errorf("platform link up: %w", err)
	}
	if replace {
		if p.defaultRoute != nil {
			_, err := c.request(ctx, unix.RTM_DELROUTE, unix.NLM_F_ACK, p.defaultRoute)
			if err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("delete platform default: %w", err)
			}
			p.defaultRoute = nil
		}
		for _, a := range old {
			if a.scope != unix.RT_SCOPE_UNIVERSE {
				continue
			}
			// Reconstruct only the address identity, not read-only dump flags
			// or lifetime attributes. Both old address families are replaced.
			_, err := c.request(ctx, unix.RTM_DELADDR, unix.NLM_F_ACK, networkAddressBody(index, a.family, a.ip, a.prefix))
			if err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("delete platform address: %w", err)
			}
		}
	}
	flags := uint16(unix.NLM_F_ACK | unix.NLM_F_CREATE | unix.NLM_F_EXCL)
	_, err := c.request(ctx, unix.RTM_NEWADDR, flags, networkAddressBody(index, cfg.family, cfg.ip, cfg.prefix))
	if err != nil {
		return fmt.Errorf("add platform address: %w", err)
	}
	if cfg.gateway != nil {
		body := platformDefaultRoute(index, cfg.family, cfg.gateway)
		if _, err := c.request(ctx, unix.RTM_NEWROUTE, flags, body); err != nil {
			return fmt.Errorf("add platform default: %w", err)
		}
		p.defaultRoute = body
	}
	return nil
}

func networkAddressBody(index int32, family int, ip net.IP, prefix int) []byte {
	body := make([]byte, unix.SizeofIfAddrmsg)
	body[0], body[1] = byte(family), byte(prefix)
	binary.NativeEndian.PutUint32(body[4:], uint32(index))
	body = nlAttr(body, unix.IFA_LOCAL, addrBytes(family, ip))
	return nlAttr(body, unix.IFA_ADDRESS, addrBytes(family, ip))
}

func platformDefaultRoute(index int32, family int, gw net.IP) []byte {
	body := make([]byte, unix.SizeofRtMsg)
	body[0], body[4], body[5], body[6], body[7] = byte(family), unix.RT_TABLE_MAIN, unix.RTPROT_BOOT, unix.RT_SCOPE_UNIVERSE, unix.RTN_UNICAST
	body = nlAttr(body, unix.RTA_GATEWAY, addrBytes(family, gw))
	body = nlAttr(body, unix.RTA_OIF, networkUint32(uint32(index)))
	// Linux treats priority zero as a wildcard during route deletion, even
	// if RTA_PRIORITY=0 is explicitly present. A nonzero metric makes the
	// remembered key exact; use IPv6's usual 1024 and 1 for IPv4.
	metric := uint32(1)
	if family == unix.AF_INET6 {
		metric = 1024
	}
	return nlAttr(body, unix.RTA_PRIORITY, networkUint32(metric))
}

func flushPlatformNeighbors(ctx context.Context, c networkRequester, index int32) error {
	body := make([]byte, unix.SizeofNdMsg)
	replies, err := c.request(ctx, unix.RTM_GETNEIGH, unix.NLM_F_DUMP, body)
	if err != nil {
		return fmt.Errorf("dump platform neighbors: %w", err)
	}
	var deletes [][]byte
	for _, r := range replies {
		if r.Type != unix.RTM_NEWNEIGH || len(r.Data) < unix.SizeofNdMsg {
			return errors.New("invalid neighbor dump reply")
		}
		p := r.Data
		if int32(binary.NativeEndian.Uint32(p[4:])) != index || p[0] != unix.AF_INET && p[0] != unix.AF_INET6 {
			continue
		}
		state := binary.NativeEndian.Uint16(p[8:])
		if state&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || p[10]&unix.NTF_PROXY != 0 {
			continue
		}
		attrs, err := parseNetworkAttrs(p[unix.SizeofNdMsg:])
		if err != nil {
			return err
		}
		dst := attrs[unix.NDA_DST]
		if len(dst) != 4 && p[0] == unix.AF_INET || len(dst) != 16 && p[0] == unix.AF_INET6 {
			return errors.New("invalid neighbor destination")
		}
		b := make([]byte, unix.SizeofNdMsg)
		b[0] = p[0]
		binary.NativeEndian.PutUint32(b[4:], uint32(index))
		deletes = append(deletes, nlAttr(b, unix.NDA_DST, dst))
	}
	var first error
	for _, b := range deletes {
		_, err := c.request(ctx, unix.RTM_DELNEIGH, unix.NLM_F_ACK, b)
		if err != nil && !errors.Is(err, unix.ENOENT) && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return first
}
