package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const (
	networkIntegrationRequired = "GUEST_RUNTIME_NETWORK_INTEGRATION_REQUIRED"
	networkIntegrationRole     = "GUEST_RUNTIME_NETWORK_TEST_ROLE"
	networkIntegrationP        = "platform0"
	networkIntegrationU        = "user0"
	networkIntegrationP4       = "192.0.2.1"
	networkIntegrationPeerP4   = "192.0.2.2"
	networkIntegrationU4       = "198.51.100.1"
	networkIntegrationPeerU4   = "198.51.100.2"
	networkIntegrationP6       = "2001:db8:144::1"
	networkIntegrationPeerP6   = "2001:db8:144::2"
	networkIntegrationU6       = "2001:db8:145::1"
	networkIntegrationPeerU6   = "2001:db8:145::2"
	networkIntegrationPort     = 18080
)

// Run as root with iproute2, nsenter, IPv6 and CONFIG_INET_DIAG_DESTROY enabled.
// Setting GUEST_RUNTIME_NETWORK_INTEGRATION_REQUIRED=1 turns unavailable test
// prerequisites into failures, so a delivery gate cannot silently pass by skip.
// Both ends of every link live in unnamed child network namespaces. A private
// mount namespace supplies the matching sysfs view used by platform.resolve.
func TestPlatformNetworkLinuxIntegration(t *testing.T) {
	switch os.Getenv(networkIntegrationRole) {
	case "peer":
		networkIntegrationPeer(t)
		return
	case "sandbox":
		networkIntegrationSandbox(t)
		return
	case "":
	default:
		t.Fatalf("invalid %s", networkIntegrationRole)
	}
	for _, tool := range []string{"ip", "nsenter"} {
		if _, err := exec.LookPath(tool); err != nil {
			networkIntegrationUnavailable(t, "%s is unavailable: %v", tool, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlatformNetworkLinuxIntegration$", "-test.v", "-test.timeout=75s")
	cmd.Env = append(os.Environ(), networkIntegrationRole+"=sandbox")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET | unix.CLONE_NEWNS, Pdeathsig: unix.SIGKILL}
	out, err := cmd.CombinedOutput()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOSYS) {
		networkIntegrationUnavailable(t, "private network/mount namespaces unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("isolated network integration: %v\n%s", err, out)
	}
	t.Logf("isolated network integration:\n%s", out)
}

func networkIntegrationUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	if os.Getenv(networkIntegrationRequired) == "1" {
		t.Fatalf("required network integration unavailable: %s", reason)
	}
	t.Skipf("%s; set %s=1 to require this gate", reason, networkIntegrationRequired)
}

func networkIntegrationSetupCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return
	}
	message := string(out)
	if strings.Contains(message, "Operation not permitted") || strings.Contains(message, "Permission denied") ||
		strings.Contains(message, "Operation not supported") || strings.Contains(message, "Address family not supported") {
		networkIntegrationUnavailable(t, "setup %s %v: %v: %s", name, args, err, out)
	}
	t.Fatalf("setup %s %v: %v\n%s", name, args, err, out)
}

func networkIntegrationSysctls(t *testing.T) {
	t.Helper()
	// The deliberate platform-source/user-egress flow has an asymmetric return
	// route. These settings are private to this test's network namespace.
	for _, path := range []string{
		"ipv4/conf/all/rp_filter", "ipv4/conf/default/rp_filter",
		"ipv6/conf/all/disable_ipv6", "ipv6/conf/default/disable_ipv6",
		"ipv6/conf/all/accept_dad", "ipv6/conf/default/accept_dad",
	} {
		if err := os.WriteFile(filepath.Join("/proc/sys/net", path), []byte("0\n"), 0); err != nil {
			networkIntegrationUnavailable(t, "private sysctl %s: %v", path, err)
		}
	}
}

func networkIntegrationSandbox(t *testing.T) {
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		networkIntegrationUnavailable(t, "private mount propagation: %v", err)
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		networkIntegrationUnavailable(t, "network namespace sysfs: %v", err)
	}
	networkIntegrationSysctls(t)
	peer := networkIntegrationStartPeer(t)
	pid := strconv.Itoa(peer.Process.Pid)
	ip := func(args ...string) { networkIntegrationSetupCommand(t, "ip", args...) }
	peerIP := func(args ...string) {
		prefix := []string{"--target", pid, "--net", "--", "ip"}
		networkIntegrationSetupCommand(t, "nsenter", append(prefix, args...)...)
	}
	ip("link", "set", "lo", "up")
	peerIP("link", "set", "lo", "up")
	for _, pair := range [][2]string{{networkIntegrationP, "ppeer"}, {networkIntegrationU, "upeer"}} {
		ip("link", "add", pair[0], "type", "veth", "peer", "name", pair[1])
		ip("link", "set", pair[1], "netns", pid)
		ip("link", "set", pair[0], "up")
		peerIP("link", "set", pair[1], "up")
	}
	for _, a := range [][3]string{
		{networkIntegrationP, networkIntegrationP4 + "/24", "-4"},
		{networkIntegrationP, networkIntegrationP6 + "/64", "-6"},
		{networkIntegrationU, networkIntegrationU4 + "/24", "-4"},
		{networkIntegrationU, networkIntegrationU6 + "/64", "-6"},
	} {
		ip(a[2], "addr", "add", a[1], "dev", a[0])
	}
	for _, a := range [][3]string{
		{"ppeer", networkIntegrationPeerP4 + "/24", "-4"},
		{"ppeer", networkIntegrationPeerP6 + "/64", "-6"},
		{"upeer", networkIntegrationPeerU4 + "/24", "-4"},
		{"upeer", networkIntegrationPeerU6 + "/64", "-6"},
	} {
		peerIP(a[2], "addr", "add", a[1], "dev", a[0])
	}
	peer.serve(t)
	device, err := os.Stat(filepath.Join("/sys/class/net", networkIntegrationP))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := currentNetworkNamespace()
	if err != nil {
		t.Fatal(err)
	}
	index, err := readIfindex(networkIntegrationP)
	if err != nil {
		t.Fatal(err)
	}
	// This injection only replaces the cold virtio driver check. Identity
	// resolution, sysfs/netns checks and every production Netlink operation run.
	guestNetwork = platformNetwork{device: device, netns: ns, index: index}
	t.Run("TCP", func(t *testing.T) { networkIntegrationTCP(t, index) })
	t.Run("Neighbors", func(t *testing.T) { networkIntegrationNeighbors(t, index) })
	t.Run("Configuration", func(t *testing.T) { networkIntegrationConfiguration(t, index) })
}

type networkIntegrationPeerProcess struct {
	*exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	stderr *bytes.Buffer
}

func networkIntegrationStartPeer(t *testing.T) *networkIntegrationPeerProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlatformNetworkLinuxIntegration$", "-test.timeout=70s")
	cmd.Env = append(os.Environ(), networkIntegrationRole+"=peer")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET, Pdeathsig: unix.SIGKILL}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		networkIntegrationUnavailable(t, "peer network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	peer := &networkIntegrationPeerProcess{Cmd: cmd, input: input, output: bufio.NewReader(output), stderr: stderr}
	peer.wait(t, "PEER_READY")
	return peer
}

func (p *networkIntegrationPeerProcess) wait(t *testing.T, expected string) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		line, err := p.output.ReadString('\n')
		ready <- result{strings.TrimSpace(line), err}
	}()
	select {
	case r := <-ready:
		if r.err != nil || r.line != expected {
			t.Fatalf("peer expected %s, got %q: %v (%s)", expected, r.line, r.err, p.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("peer did not report %s", expected)
	}
}

func (p *networkIntegrationPeerProcess) serve(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(p.input, "SERVE\n"); err != nil {
		t.Fatal(err)
	}
	p.wait(t, "PEER_SERVING")
}

func networkIntegrationPeer(t *testing.T) {
	networkIntegrationSysctls(t)
	fmt.Println("PEER_READY")
	commands := bufio.NewScanner(os.Stdin)
	if !commands.Scan() || commands.Text() != "SERVE" {
		t.Fatal("peer did not receive SERVE")
	}
	for _, address := range []string{networkIntegrationPeerP4, networkIntegrationPeerU4, networkIntegrationPeerP6, networkIntegrationPeerU6} {
		family := "tcp6"
		if net.ParseIP(address).To4() != nil {
			family = "tcp4"
		}
		networkIntegrationEcho(t, family, net.JoinHostPort(address, strconv.Itoa(networkIntegrationPort)))
	}
	fmt.Println("PEER_SERVING")
	for commands.Scan() {
		t.Fatalf("unexpected peer command %q", commands.Text())
	}
}

func networkIntegrationEcho(t *testing.T, family, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(family, address)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	connections := make(map[net.Conn]struct{})
	closed := false
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for conn := range connections {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			connections[conn] = struct{}{}
			mu.Unlock()
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
				mu.Lock()
				delete(connections, conn)
				mu.Unlock()
			}()
		}
	}()
	return listener
}

func networkIntegrationDial(t *testing.T, family, source, destination string, port int) net.Conn {
	t.Helper()
	dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	conn, err := dialer.Dial(family, net.JoinHostPort(destination, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial %s from %s to %s: %v", family, source, destination, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func networkIntegrationMappedDial(t *testing.T) net.Conn {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "mapped-tcp-integration")
	defer file.Close()
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
		t.Fatal(err)
	}
	local := &unix.SockaddrInet6{}
	copy(local.Addr[:], net.ParseIP("::ffff:"+networkIntegrationP4).To16())
	if err := unix.Bind(fd, local); err != nil {
		t.Fatal(err)
	}
	remote := &unix.SockaddrInet6{Port: networkIntegrationPort}
	copy(remote.Addr[:], net.ParseIP("::ffff:"+networkIntegrationPeerP4).To16())
	if err := unix.Connect(fd, remote); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		t.Fatal(err)
	}
	if n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}, 2000); err != nil || n != 1 {
		t.Fatalf("mapped TCP connect: ready=%d err=%v", n, err)
	}
	if errno, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR); err != nil || errno != 0 {
		t.Fatalf("mapped TCP connect: errno=%d err=%v", errno, err)
	}
	conn, err := net.FileConn(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func networkIntegrationRoundTrip(t *testing.T, conn net.Conn, label string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := []byte(label + "\n")
	if _, err := conn.Write(message); err != nil {
		t.Fatalf("%s write: %v", label, err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, message) {
		t.Fatalf("%s echo=%q err=%v", label, got, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func networkIntegrationReleased(t *testing.T, conn net.Conn, label string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err := conn.Read(b[:])
	var netErr net.Error
	if err == nil || errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("%s remained open after exact-cookie destroy: %v", label, err)
	}
}

func networkIntegrationConn(t *testing.T, protocol int) *networkConn {
	t.Helper()
	conn, err := openNetworkConn(protocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.close)
	return conn
}

func networkIntegrationAddresses(t *testing.T, index int32) []platformAddress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addresses, err := networkAddresses(ctx, networkIntegrationConn(t, unix.NETLINK_ROUTE), index)
	if err != nil {
		t.Fatal(err)
	}
	return addresses
}

func networkIntegrationTCP(t *testing.T, index int32) {
	targets := []struct {
		label  string
		family uint8
		conn   net.Conn
	}{
		{"platform IPv4", unix.AF_INET, networkIntegrationDial(t, "tcp4", networkIntegrationP4, networkIntegrationPeerP4, networkIntegrationPort)},
		{"platform IPv6", unix.AF_INET6, networkIntegrationDial(t, "tcp6", networkIntegrationP6, networkIntegrationPeerP6, networkIntegrationPort)},
		{"mapped IPv6", unix.AF_INET6, networkIntegrationMappedDial(t)},
	}
	loopListener := networkIntegrationEcho(t, "tcp4", "127.0.0.1:0")
	localListener := networkIntegrationEcho(t, "tcp4", net.JoinHostPort(networkIntegrationP4, "0"))
	preserved := map[string]net.Conn{
		"user IPv4":                              networkIntegrationDial(t, "tcp4", networkIntegrationU4, networkIntegrationPeerU4, networkIntegrationPort),
		"user IPv6":                              networkIntegrationDial(t, "tcp6", networkIntegrationU6, networkIntegrationPeerU6, networkIntegrationPort),
		"platform source through user interface": networkIntegrationDial(t, "tcp4", networkIntegrationP4, networkIntegrationPeerU4, networkIntegrationPort),
		"loopback":                               networkIntegrationDial(t, "tcp4", "127.0.0.1", "127.0.0.1", loopListener.Addr().(*net.TCPAddr).Port),
		"local platform address":                 networkIntegrationDial(t, "tcp4", networkIntegrationP4, networkIntegrationP4, localListener.Addr().(*net.TCPAddr).Port),
	}
	wanted := make(map[string]string)
	for _, target := range targets {
		networkIntegrationRoundTrip(t, target.conn, target.label)
		wanted[fmt.Sprintf("%d/%d/%d", target.family, target.conn.LocalAddr().(*net.TCPAddr).Port, networkIntegrationPort)] = target.label
	}
	for label, conn := range preserved {
		networkIntegrationRoundTrip(t, conn, label)
	}
	var ips []net.IP
	for _, address := range networkIntegrationAddresses(t, index) {
		ips = append(ips, address.ip)
	}
	route := networkIntegrationConn(t, unix.NETLINK_ROUTE)
	diag := networkIntegrationConn(t, unix.NETLINK_SOCK_DIAG)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	candidates, err := collectPlatformTCP(ctx, route, diag, index, ips)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(wanted) {
		t.Fatalf("selected %d TCP sockets, wanted exactly %d: %+v", len(candidates), len(wanted), candidates)
	}
	for _, candidate := range candidates {
		key := fmt.Sprintf("%d/%d/%d", candidate.family, binary.BigEndian.Uint16(candidate.id[:2]), binary.BigEndian.Uint16(candidate.id[2:4]))
		if _, exists := wanted[key]; !exists {
			t.Fatalf("selected a preserved or unexpected socket: %s %+v", key, candidate)
		}
		delete(wanted, key)
	}
	if len(wanted) != 0 {
		t.Fatalf("platform sockets were not selected: %v", wanted)
	}
	result := releasePlatformTCP(ctx, diag, candidates)
	if result.Selected != 3 || result.Released != 3 || result.Vanished != 0 || result.Failed != 0 || result.Skipped != 0 || result.Err != nil {
		t.Fatalf("exact-cookie release: %+v", result)
	}
	t.Logf("real TCP collection and three exact-cookie releases: %s", time.Since(start))
	for _, target := range targets {
		networkIntegrationReleased(t, target.conn, target.label)
	}
	for label, conn := range preserved {
		networkIntegrationRoundTrip(t, conn, label+" after cleanup")
	}
	// Accept another connection to prove the listener itself remains usable.
	newLocal := networkIntegrationDial(t, "tcp4", networkIntegrationP4, networkIntegrationP4, localListener.Addr().(*net.TCPAddr).Port)
	networkIntegrationRoundTrip(t, newLocal, "listener after cleanup")
}

func networkIntegrationNeighborKey(index int32, ip string) string {
	return fmt.Sprintf("%d/%s", index, ip)
}

func networkIntegrationNeighborStates(t *testing.T) map[string]uint16 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	replies, err := networkIntegrationConn(t, unix.NETLINK_ROUTE).request(ctx, unix.RTM_GETNEIGH, unix.NLM_F_DUMP, make([]byte, unix.SizeofNdMsg))
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]uint16)
	for _, reply := range replies {
		if reply.Type != unix.RTM_NEWNEIGH || len(reply.Data) < unix.SizeofNdMsg {
			t.Fatalf("invalid neighbor observation: %+v", reply)
		}
		body := reply.Data
		if body[0] != unix.AF_INET && body[0] != unix.AF_INET6 {
			continue
		}
		attrs, err := parseNetworkAttrs(body[unix.SizeofNdMsg:])
		if err != nil {
			t.Fatal(err)
		}
		index := int32(binary.NativeEndian.Uint32(body[4:8]))
		states[networkIntegrationNeighborKey(index, net.IP(attrs[unix.NDA_DST]).String())] = binary.NativeEndian.Uint16(body[8:10])
	}
	return states
}

func networkIntegrationNeighbors(t *testing.T, index int32) {
	user, err := readIfindex(networkIntegrationU)
	if err != nil {
		t.Fatal(err)
	}
	entries := []struct {
		device, ip, state string
		index             int32
		keep              bool
		wantState         uint16
	}{
		{networkIntegrationP, "192.0.2.77", "stale", index, false, unix.NUD_STALE},
		{networkIntegrationP, "192.0.2.78", "permanent", index, true, unix.NUD_PERMANENT},
		{networkIntegrationP, "192.0.2.79", "noarp", index, true, unix.NUD_NOARP},
		{networkIntegrationP, "2001:db8:144::77", "stale", index, false, unix.NUD_STALE},
		{networkIntegrationP, "2001:db8:144::78", "permanent", index, true, unix.NUD_PERMANENT},
		{networkIntegrationU, "198.51.100.77", "stale", user, true, unix.NUD_STALE},
		{networkIntegrationU, "2001:db8:145::77", "stale", user, true, unix.NUD_STALE},
	}
	for _, entry := range entries {
		networkIntegrationSetupCommand(t, "ip", "neigh", "replace", entry.ip, "lladdr", "02:00:00:00:01:44", "nud", entry.state, "dev", entry.device)
	}
	before := networkIntegrationNeighborStates(t)
	for _, entry := range entries {
		if before[networkIntegrationNeighborKey(entry.index, entry.ip)] != entry.wantState {
			t.Fatalf("neighbor fixture missing/wrong state: %+v (%v)", entry, before)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := flushPlatformNeighbors(ctx, networkIntegrationConn(t, unix.NETLINK_ROUTE), index); err != nil {
		t.Fatal(err)
	}
	after := networkIntegrationNeighborStates(t)
	for _, entry := range entries {
		state, exists := after[networkIntegrationNeighborKey(entry.index, entry.ip)]
		if exists != entry.keep || exists && state != entry.wantState {
			t.Fatalf("neighbor cleanup: %+v observed exists=%v state=%#x", entry, exists, state)
		}
	}
}

type networkIntegrationDefault struct {
	family int
	index  int32
	gw     string
	metric uint32
}

func networkIntegrationDefaults(t *testing.T) map[networkIntegrationDefault]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn := networkIntegrationConn(t, unix.NETLINK_ROUTE)
	defaults := make(map[networkIntegrationDefault]int)
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		body := make([]byte, unix.SizeofRtMsg)
		body[0] = byte(family)
		replies, err := conn.request(ctx, unix.RTM_GETROUTE, unix.NLM_F_DUMP, body)
		if err != nil {
			t.Fatal(err)
		}
		for _, reply := range replies {
			if reply.Type != unix.RTM_NEWROUTE || len(reply.Data) < unix.SizeofRtMsg {
				t.Fatalf("invalid route observation: %+v", reply)
			}
			body := reply.Data
			if body[1] != 0 || body[2] != 0 || body[7] != unix.RTN_UNICAST || body[4] != unix.RT_TABLE_MAIN {
				continue
			}
			attrs, err := parseNetworkAttrs(body[unix.SizeofRtMsg:])
			if err != nil {
				t.Fatal(err)
			}
			if len(attrs[unix.RTA_OIF]) != 4 {
				t.Fatalf("default route lacks OIF: %x", body)
			}
			key := networkIntegrationDefault{family: int(body[0]), index: int32(binary.NativeEndian.Uint32(attrs[unix.RTA_OIF])), gw: net.IP(attrs[unix.RTA_GATEWAY]).String()}
			if metric, exists := attrs[unix.RTA_PRIORITY]; exists {
				if len(metric) != 4 {
					t.Fatalf("invalid route metric: %x", metric)
				}
				key.metric = binary.NativeEndian.Uint32(metric)
			}
			defaults[key]++
		}
	}
	return defaults
}

func networkIntegrationAssertConfiguration(t *testing.T, index int32, address string, prefix, defaultFamily int) {
	t.Helper()
	global := 0
	for _, a := range networkIntegrationAddresses(t, index) {
		if a.scope != unix.RT_SCOPE_UNIVERSE {
			continue
		}
		global++
		if !a.ip.Equal(net.ParseIP(address)) || a.prefix != prefix {
			t.Fatalf("unexpected platform global address: %s/%d (wanted %s/%d)", a.ip, a.prefix, address, prefix)
		}
	}
	if global != 1 {
		t.Fatalf("platform has %d global addresses, wanted one", global)
	}
	user, err := readIfindex(networkIntegrationU)
	if err != nil {
		t.Fatal(err)
	}
	userAddresses := make(map[string]bool)
	for _, a := range networkIntegrationAddresses(t, user) {
		if a.scope == unix.RT_SCOPE_UNIVERSE {
			userAddresses[a.ip.String()] = true
		}
	}
	if len(userAddresses) != 2 || !userAddresses[networkIntegrationU4] || !userAddresses[networkIntegrationU6] {
		t.Fatalf("user interface addresses changed: %v", userAddresses)
	}
	defaults := networkIntegrationDefaults(t)
	for _, entry := range []networkIntegrationDefault{{unix.AF_INET, user, networkIntegrationPeerU4, 700}, {unix.AF_INET6, user, networkIntegrationPeerU6, 700}} {
		if defaults[entry] != 1 {
			t.Fatalf("user default changed: %+v in %v", entry, defaults)
		}
		delete(defaults, entry)
	}
	if defaultFamily == 0 {
		if len(defaults) != 0 {
			t.Fatalf("empty gateway retained a platform default: %v", defaults)
		}
	} else {
		gateway := networkIntegrationPeerP4
		metric := uint32(1)
		if defaultFamily == unix.AF_INET6 {
			gateway = networkIntegrationPeerP6
			metric = 1024
		}
		key := networkIntegrationDefault{defaultFamily, index, gateway, metric}
		if len(defaults) != 1 || defaults[key] != 1 {
			t.Fatalf("incorrect platform default: %v, wanted %+v", defaults, key)
		}
	}
	iface, err := net.InterfaceByIndex(int(index))
	if err != nil || iface.MTU != 1400 {
		t.Fatalf("platform MTU: interface=%+v err=%v", iface, err)
	}
}

func networkIntegrationConfiguration(t *testing.T, index int32) {
	for _, route := range [][3]string{{"-4", networkIntegrationPeerU4, networkIntegrationU}, {"-6", networkIntegrationPeerU6, networkIntegrationU}} {
		networkIntegrationSetupCommand(t, "ip", route[0], "route", "add", "default", "via", route[1], "dev", route[2], "metric", "700")
	}
	steps := []struct {
		name, address, cidr, gateway string
		prefix, defaultFamily        int
	}{
		{"same IPv4", networkIntegrationP4, networkIntegrationP4 + "/24", networkIntegrationPeerP4, 24, unix.AF_INET},
		{"same IPv4 again", networkIntegrationP4, networkIntegrationP4 + "/24", networkIntegrationPeerP4, 24, unix.AF_INET},
		{"empty IPv4 gateway", networkIntegrationP4, networkIntegrationP4 + "/24", "", 24, 0},
		{"IPv4 to IPv6", networkIntegrationP6, networkIntegrationP6 + "/64", networkIntegrationPeerP6, 64, unix.AF_INET6},
		{"empty IPv6 gateway", networkIntegrationP6, networkIntegrationP6 + "/64", "", 64, 0},
		{"IPv6 to IPv4", networkIntegrationP4, networkIntegrationP4 + "/24", networkIntegrationPeerP4, 24, unix.AF_INET},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			spec := &proto.NetworkSpec{IPCIDR: step.cidr, Nexthop: step.gateway, MTU: 1400}
			if err := applyNetworkReplace(spec); err != nil {
				t.Fatal(err)
			}
			networkIntegrationAssertConfiguration(t, index, step.address, step.prefix, step.defaultFamily)
		})
		if t.Failed() {
			return
		}
	}
	for _, tc := range []struct {
		name string
		spec *proto.NetworkSpec
	}{{"nil spec", nil}, {"empty spec", &proto.NetworkSpec{}}} {
		t.Run(tc.name, func(t *testing.T) {
			conn := networkIntegrationDial(t, "tcp4", networkIntegrationP4, networkIntegrationPeerP4, networkIntegrationPort)
			networkIntegrationRoundTrip(t, conn, tc.name+" before restore")
			start := time.Now()
			if err := applyNetworkReplace(tc.spec); err != nil {
				t.Fatal(err)
			}
			t.Logf("real Restore network path with %s: %s", tc.name, time.Since(start))
			networkIntegrationReleased(t, conn, tc.name)
			networkIntegrationAssertConfiguration(t, index, networkIntegrationP4, 24, unix.AF_INET)
		})
	}
	t.Run("missing owned default keeps another metric", func(t *testing.T) {
		// Linux treats a zero deletion metric as a wildcard. Exercise the
		// production-recorded deletion body after its exact owned entry has
		// disappeared, while a route with the same OIF and gateway remains.
		networkIntegrationSetupCommand(t, "ip", "-4", "route", "add", "default", "via", networkIntegrationPeerP4, "dev", networkIntegrationP, "metric", "701")
		networkIntegrationSetupCommand(t, "ip", "-4", "route", "del", "default", "via", networkIntegrationPeerP4, "dev", networkIntegrationP, "metric", "1")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := networkIntegrationConn(t, unix.NETLINK_ROUTE).request(ctx, unix.RTM_DELROUTE, unix.NLM_F_ACK, guestNetwork.defaultRoute)
		if !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			t.Fatalf("missing owned default deletion: %v", err)
		}
		defaults := networkIntegrationDefaults(t)
		other := networkIntegrationDefault{unix.AF_INET, index, networkIntegrationPeerP4, 701}
		if defaults[other] != 1 {
			t.Fatalf("deleting a missing owned default removed another metric: %v", defaults)
		}
		if err := applyNetworkReplace(&proto.NetworkSpec{IPCIDR: networkIntegrationP4 + "/24", MTU: 1400}); err != nil {
			t.Fatal(err)
		}
		defaults = networkIntegrationDefaults(t)
		if defaults[other] != 1 {
			t.Fatalf("same-IP empty-gateway Restore removed another metric: %v", defaults)
		}
	})
}
