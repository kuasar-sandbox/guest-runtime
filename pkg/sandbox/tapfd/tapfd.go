// Package tapfd is sandbox-ctl's consumer side of the tapfd handoff
// protocol (docs/tapfd.md): it execs a provider helper and receives a tap
// queue fd (with virtio-net header) plus metadata over a unix socket via
// SCM_RIGHTS, so CH can drive virtio-net off a fd the network provider owns.
//
// Self-contained: stdlib + golang.org/x/sys/unix only.
package tapfd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultTimeout bounds the whole handoff (exec helper → recv fd → helper exit).
const DefaultTimeout = 5 * time.Second

// Metadata is the recognized subset of the handoff payload (docs/tapfd.md
// §4.3). Unknown keys are ignored per the protocol's forward-compat rule.
type Metadata struct {
	MAC string // provider-assigned MAC the VMM must mirror onto virtio-net
	IP  string // interface L3 address (may be bare, no mask)
	MTU int    // interface MTU (0 = absent)
}

// Acquire runs the §5 exec-helper handoff: it creates a connected socketpair,
// execs argv with TAPFD_SOCKET=fd=3 (the helper's inherited end) and
// TAPFD_WANT_NETNS=1 (§5.3.1 — request the tap's netns fd), receives exactly
// one tap fd + metadata + an optional netns fd, and requires the helper to exit
// 0. The whole exchange is bounded by timeout (<=0 → DefaultTimeout). On success
// the caller owns the returned files: tap (hand to CH via cmd.ExtraFiles) and,
// when the provider's tap is netns-isolated, netns (nil otherwise — used to
// launch CH inside the tap's network namespace, §4.6). Close both after the run.
func Acquire(ctx context.Context, argv []string, timeout time.Duration) (tap *os.File, netns *os.File, meta Metadata, err error) {
	if len(argv) == 0 {
		return nil, nil, Metadata{}, errors.New("tapfd: empty exec argv")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: socketpair: %w", err)
	}
	recvFile := os.NewFile(uintptr(sp[0]), "tapfd-recv")
	helperFile := os.NewFile(uintptr(sp[1]), "tapfd-helper")

	// net.FileConn dups recvFile into its own fd, so close our copy after.
	conn, err := net.FileConn(recvFile)
	_ = recvFile.Close()
	if err != nil {
		_ = helperFile.Close()
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: fileconn: %w", err)
	}
	uconn := conn.(*net.UnixConn)
	defer uconn.Close()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	// helperFile becomes the child's fd 3 (cmd.ExtraFiles[0]); the helper dials
	// it because we point TAPFD_SOCKET at it (docs/tapfd.md §5.3).
	// TAPFD_WANT_NETNS=1 requests the tap's netns fd (§5.3.1) so we can launch
	// CH inside it; a provider whose tap isn't netns-isolated simply omits it.
	cmd.Env = append(os.Environ(), "TAPFD_SOCKET=fd=3", "TAPFD_WANT_NETNS=1")
	cmd.ExtraFiles = []*os.File{helperFile}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = helperFile.Close()
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: exec %s: %w", argv[0], err)
	}
	_ = helperFile.Close() // the child holds its own copy now

	_ = uconn.SetDeadline(time.Now().Add(timeout))
	tapFiles, netnsFile, meta, rerr := RecvFd(uconn)
	werr := cmd.Wait() // helper sends one message then exits (§5.4)

	if rerr != nil {
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: recv from helper %s: %w (helper exit: %v, stderr=%q)",
			argv[0], rerr, werr, strings.TrimSpace(stderr.String()))
	}
	if werr != nil { // §5.1: non-zero exit or timeout ⇒ failure, do not use the nic
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: helper %s exited non-zero: %w (stderr=%q)",
			argv[0], werr, strings.TrimSpace(stderr.String()))
	}
	if len(tapFiles) != 1 { // single-queue v1
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: expected 1 tap fd, received %d", len(tapFiles))
	}
	return tapFiles[0], netnsFile, meta, nil
}

// RecvFd performs the §4.4 receive on a connected unix socket: one recvmsg,
// collect every SCM_RIGHTS fd (so none leak), parse the NUL-terminated metadata
// line, cross-check the declared fd= + netns_fd= count against what arrived,
// then split by position — the first fd= are tap queues, the trailing netns_fd=
// (0 or 1) is the tap's netns reference (docs/tapfd.md §4.6). netnsFile is nil
// when the provider sends none. Any error closes all received fds.
func RecvFd(conn *net.UnixConn) (tapFiles []*os.File, netnsFile *os.File, meta Metadata, err error) {
	buf := make([]byte, 512)
	oob := make([]byte, unix.CmsgSpace(8*4)) // up to 8 ints: multi-queue + trailing netns (§4.4 step 1)
	n, oobn, _, _, rerr := conn.ReadMsgUnix(buf, oob)
	if rerr != nil {
		return nil, nil, Metadata{}, fmt.Errorf("recvmsg: %w", rerr)
	}

	// Collect ALL fds from every SCM_RIGHTS control message (§4.4 step 2).
	var fds []int
	if scms, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
		for i := range scms {
			if rs, e := unix.ParseUnixRights(&scms[i]); e == nil {
				fds = append(fds, rs...)
			}
		}
	}

	m, fdCount, netnsCount, perr := parsePayload(buf[:n])
	if perr != nil {
		closeAllInts(fds)
		return nil, nil, Metadata{}, perr
	}
	if len(fds) == 0 {
		return nil, nil, Metadata{}, errors.New("tapfd: no fd in SCM_RIGHTS")
	}
	if want := fdCount + netnsCount; len(fds) != want {
		closeAllInts(fds)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: payload fd=%d netns_fd=%d but received %d fds", fdCount, netnsCount, len(fds))
	}

	// Split by position (§4.4 step 5): first fdCount = tap queues, trailing
	// netnsCount (0 or 1) = the tap's netns.
	tapFiles = make([]*os.File, fdCount)
	for i := 0; i < fdCount; i++ {
		tapFiles[i] = os.NewFile(uintptr(fds[i]), "tapfd-queue")
	}
	if netnsCount > 0 {
		netnsFile = os.NewFile(uintptr(fds[fdCount]), "tapfd-netns")
	}
	return tapFiles, netnsFile, m, nil
}

// parsePayload parses the metadata line up to the first NUL (§4.3), returning
// the recognized fields, the required fd= count, and the optional netns_fd=
// count (0 or 1, §4.3). Unknown keys are ignored.
func parsePayload(b []byte) (Metadata, int, int, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	var m Metadata
	fdCount := 0
	netnsCount := 0
	haveFD := false
	for _, tok := range strings.Fields(string(b)) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return Metadata{}, 0, 0, fmt.Errorf("tapfd: token %q has no '='", tok)
		}
		switch k {
		case "mac":
			m.MAC = v
		case "ip":
			m.IP = v
		case "mtu":
			m.MTU, _ = strconv.Atoi(v) // best-effort; absent/garbage → 0
		case "fd":
			c, err := strconv.Atoi(v)
			if err != nil || c < 1 {
				return Metadata{}, 0, 0, fmt.Errorf("tapfd: invalid fd=%q", v)
			}
			fdCount, haveFD = c, true
		case "netns_fd":
			c, err := strconv.Atoi(v)
			if err != nil || c < 0 || c > 1 {
				return Metadata{}, 0, 0, fmt.Errorf("tapfd: invalid netns_fd=%q (want 0 or 1)", v)
			}
			netnsCount = c
		}
	}
	if !haveFD {
		return Metadata{}, 0, 0, errors.New("tapfd: payload missing required fd= count")
	}
	return m, fdCount, netnsCount, nil
}

func closeAllInts(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}
