// Guest IP-layer setup via raw netlink. Replaces the kernel's
// `ip=...` cmdline + CONFIG_IP_PNP path so we can drop those from
// the kernel.
//
// Three RTNETLINK messages are sufficient for static-IP setup:
//   1. RTM_NEWLINK with IFF_UP — bring iface up
//   2. RTM_NEWADDR with IFA_LOCAL/IFA_ADDRESS — assign the IP
//   3. RTM_NEWROUTE with RTA_GATEWAY — install default route (optional)
//
// Each carries NLM_F_REQUEST | NLM_F_ACK; we drain the ack and verify
// the kernel accepted (NLMSG_ERROR with code 0). Any non-zero error
// short-circuits and bubbles up.
//
// Implementation uses raw `golang.org/x/sys/unix` syscalls — already
// a dependency. ~250 lines, no new third-party libs.

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
	"golang.org/x/sys/unix"
)

// applyNetwork executes hostname + bring-iface-up + assign-IP + default-route
// in order. Any step's failure aborts subsequent steps and returns the error.
func applyNetwork(spec *proto.NetworkSpec) error {
	if spec == nil {
		return nil
	}
	if spec.Hostname != "" {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("sethostname %q: %w", spec.Hostname, err)
		}
	}
	if spec.IPCIDR == "" {
		return nil
	}
	iface := spec.Interface
	if iface == "" {
		iface = "eth0"
	}
	ifindex, err := readIfindex(iface)
	if err != nil {
		return fmt.Errorf("ifindex %s: %w", iface, err)
	}

	ip, ipnet, err := net.ParseCIDR(spec.IPCIDR)
	if err != nil {
		return fmt.Errorf("parse cidr %q: %w", spec.IPCIDR, err)
	}
	prefixLen, _ := ipnet.Mask.Size()
	family := unix.AF_INET
	if ip.To4() == nil {
		family = unix.AF_INET6
	}

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	seq := uint32(1)

	if err := nlSendLinkUp(fd, seq, ifindex); err != nil {
		return fmt.Errorf("link up %s: %w", iface, err)
	}
	seq++
	if err := nlSendAddrAdd(fd, seq, ifindex, family, ip, prefixLen); err != nil {
		return fmt.Errorf("addr add %s on %s: %w", spec.IPCIDR, iface, err)
	}
	seq++

	if spec.Gateway != "" {
		gw := net.ParseIP(spec.Gateway)
		if gw == nil {
			return fmt.Errorf("parse gateway %q: invalid IP", spec.Gateway)
		}
		gwFamily := unix.AF_INET
		if gw.To4() == nil {
			gwFamily = unix.AF_INET6
		}
		if err := nlSendDefaultRoute(fd, seq, ifindex, gwFamily, gw); err != nil {
			return fmt.Errorf("default route via %s: %w", spec.Gateway, err)
		}
	}
	return nil
}

// readIfindex returns the kernel-assigned interface index for name via
// sysfs (one open+read+parse, no netlink round-trip).
func readIfindex(name string) (int32, error) {
	data, err := os.ReadFile("/sys/class/net/" + name + "/ifindex")
	if err != nil {
		return 0, err
	}
	idx, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return int32(idx), nil
}

// --- netlink message construction ---------------------------------

const nlAlignTo = 4

// nlAlign rounds len up to 4 bytes (NLMSG_ALIGN).
func nlAlign(n int) int { return (n + nlAlignTo - 1) &^ (nlAlignTo - 1) }

// nlAttr appends a TLV attribute to buf and returns the new buf.
func nlAttr(buf []byte, attrType uint16, value []byte) []byte {
	hdrSize := 4 // sizeof(rtattr)
	totalLen := hdrSize + len(value)
	padded := nlAlign(totalLen)
	a := make([]byte, padded)
	binary.LittleEndian.PutUint16(a[0:2], uint16(totalLen))
	binary.LittleEndian.PutUint16(a[2:4], attrType)
	copy(a[4:], value)
	return append(buf, a...)
}

// nlSend sends a single message with NLM_F_REQUEST|NLM_F_ACK and waits
// for the kernel ack (NLMSG_ERROR with err==0).
func nlSend(fd int, msgType uint16, flags uint16, seq uint32, body []byte) error {
	const hdrSize = unix.SizeofNlMsghdr
	totalLen := hdrSize + len(body)
	buf := make([]byte, totalLen)
	hdr := (*unix.NlMsghdr)(unsafe.Pointer(&buf[0]))
	hdr.Len = uint32(totalLen)
	hdr.Type = msgType
	hdr.Flags = unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags
	hdr.Seq = seq
	hdr.Pid = 0
	copy(buf[hdrSize:], body)

	if err := unix.Sendto(fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}

	rbuf := make([]byte, 4096)
	for {
		n, _, err := unix.Recvfrom(fd, rbuf, 0)
		if err != nil {
			return fmt.Errorf("recvfrom: %w", err)
		}
		if n < hdrSize {
			return fmt.Errorf("short recv %d", n)
		}
		rhdr := (*unix.NlMsghdr)(unsafe.Pointer(&rbuf[0]))
		if rhdr.Type == unix.NLMSG_ERROR {
			// payload is int32 errno, then echoed request hdr
			errno := int32(binary.LittleEndian.Uint32(rbuf[hdrSize : hdrSize+4]))
			if errno == 0 {
				return nil
			}
			return fmt.Errorf("netlink error %d (%s)", -errno, unix.Errno(-errno))
		}
		if rhdr.Flags&unix.NLM_F_MULTI != 0 && rhdr.Type != unix.NLMSG_DONE {
			continue
		}
		// unexpected reply (multipart on a non-multipart op)
		return fmt.Errorf("unexpected netlink reply type %d", rhdr.Type)
	}
}

// nlSendLinkUp issues RTM_NEWLINK with IFF_UP/IFF_UP set on ifindex.
func nlSendLinkUp(fd int, seq uint32, ifindex int32) error {
	body := make([]byte, unix.SizeofIfInfomsg)
	ifi := (*unix.IfInfomsg)(unsafe.Pointer(&body[0]))
	ifi.Family = unix.AF_UNSPEC
	ifi.Index = ifindex
	ifi.Flags = unix.IFF_UP
	ifi.Change = unix.IFF_UP
	return nlSend(fd, unix.RTM_NEWLINK, 0, seq, body)
}

// nlSendAddrAdd issues RTM_NEWADDR with IFA_LOCAL + IFA_ADDRESS.
func nlSendAddrAdd(fd int, seq uint32, ifindex int32, family int, ip net.IP, prefix int) error {
	body := make([]byte, unix.SizeofIfAddrmsg)
	ifa := (*unix.IfAddrmsg)(unsafe.Pointer(&body[0]))
	ifa.Family = uint8(family)
	ifa.Prefixlen = uint8(prefix)
	ifa.Index = uint32(ifindex)
	ifa.Scope = unix.RT_SCOPE_UNIVERSE

	var raw []byte
	if family == unix.AF_INET {
		raw = ip.To4()
	} else {
		raw = ip.To16()
	}
	body = nlAttr(body, unix.IFA_LOCAL, raw)
	body = nlAttr(body, unix.IFA_ADDRESS, raw)

	return nlSend(fd, unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, seq, body)
}

// nlSendDefaultRoute issues RTM_NEWROUTE for 0.0.0.0/0 (or ::/0) via gw on ifindex.
func nlSendDefaultRoute(fd int, seq uint32, ifindex int32, family int, gw net.IP) error {
	body := make([]byte, unix.SizeofRtMsg)
	rt := (*unix.RtMsg)(unsafe.Pointer(&body[0]))
	rt.Family = uint8(family)
	rt.Dst_len = 0 // 0.0.0.0/0 or ::/0
	rt.Src_len = 0
	rt.Tos = 0
	rt.Table = unix.RT_TABLE_MAIN
	rt.Protocol = unix.RTPROT_BOOT
	rt.Scope = unix.RT_SCOPE_UNIVERSE
	rt.Type = unix.RTN_UNICAST
	rt.Flags = 0

	var raw []byte
	if family == unix.AF_INET {
		raw = gw.To4()
	} else {
		raw = gw.To16()
	}
	body = nlAttr(body, unix.RTA_GATEWAY, raw)

	// OIF (output interface) — 4 bytes LE int32 in NlAttr
	oifBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(oifBytes, uint32(ifindex))
	body = nlAttr(body, unix.RTA_OIF, oifBytes)

	return nlSend(fd, unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, seq, body)
}

// _ keeps the syscall import live in case future revisions need raw fcntls.
var _ = syscall.SOCK_STREAM
