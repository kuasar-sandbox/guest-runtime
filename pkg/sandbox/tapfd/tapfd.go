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
// execs argv with TAPFD_SOCKET=fd=3 (the helper's inherited end), receives
// exactly one tap fd + metadata, and requires the helper to exit 0. The whole
// exchange is bounded by timeout (<=0 → DefaultTimeout). On success the caller
// owns the returned *os.File (hand to CH via cmd.ExtraFiles, then close).
func Acquire(ctx context.Context, argv []string, timeout time.Duration) (*os.File, Metadata, error) {
	if len(argv) == 0 {
		return nil, Metadata{}, errors.New("tapfd: empty exec argv")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("tapfd: socketpair: %w", err)
	}
	recvFile := os.NewFile(uintptr(sp[0]), "tapfd-recv")
	helperFile := os.NewFile(uintptr(sp[1]), "tapfd-helper")

	// net.FileConn dups recvFile into its own fd, so close our copy after.
	conn, err := net.FileConn(recvFile)
	_ = recvFile.Close()
	if err != nil {
		_ = helperFile.Close()
		return nil, Metadata{}, fmt.Errorf("tapfd: fileconn: %w", err)
	}
	uconn := conn.(*net.UnixConn)
	defer uconn.Close()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	// helperFile becomes the child's fd 3 (cmd.ExtraFiles[0]); the helper
	// dials it because we point TAPFD_SOCKET at it (docs/tapfd.md §5.3).
	cmd.Env = append(os.Environ(), "TAPFD_SOCKET=fd=3")
	cmd.ExtraFiles = []*os.File{helperFile}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = helperFile.Close()
		return nil, Metadata{}, fmt.Errorf("tapfd: exec %s: %w", argv[0], err)
	}
	_ = helperFile.Close() // the child holds its own copy now

	_ = uconn.SetDeadline(time.Now().Add(timeout))
	files, meta, rerr := RecvFd(uconn)
	werr := cmd.Wait() // helper sends one message then exits (§5.4)

	if rerr != nil {
		closeAll(files)
		return nil, Metadata{}, fmt.Errorf("tapfd: recv from helper %s: %w (helper exit: %v, stderr=%q)",
			argv[0], rerr, werr, strings.TrimSpace(stderr.String()))
	}
	if werr != nil { // §5.1: non-zero exit or timeout ⇒ failure, do not use the nic
		closeAll(files)
		return nil, Metadata{}, fmt.Errorf("tapfd: helper %s exited non-zero: %w (stderr=%q)",
			argv[0], werr, strings.TrimSpace(stderr.String()))
	}
	if len(files) != 1 { // single-queue v1
		closeAll(files)
		return nil, Metadata{}, fmt.Errorf("tapfd: expected 1 fd, received %d", len(files))
	}
	return files[0], meta, nil
}

// RecvFd performs the §4.4 receive on a connected unix socket: one recvmsg,
// collect every SCM_RIGHTS fd (so none leak), parse the NUL-terminated
// metadata line, and cross-check the declared fd= count against what arrived.
// Any error closes all received fds before returning.
func RecvFd(conn *net.UnixConn) ([]*os.File, Metadata, error) {
	buf := make([]byte, 512)
	oob := make([]byte, unix.CmsgSpace(4*4)) // up to 4 ints of ancillary
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("recvmsg: %w", err)
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
	fdFiles := func() []*os.File {
		fs := make([]*os.File, len(fds))
		for i, fd := range fds {
			fs[i] = os.NewFile(uintptr(fd), "tapfd-queue")
		}
		return fs
	}

	meta, fdCount, perr := parsePayload(buf[:n])
	if perr != nil {
		closeAllInts(fds)
		return nil, Metadata{}, perr
	}
	if len(fds) == 0 {
		return nil, Metadata{}, errors.New("tapfd: no fd in SCM_RIGHTS")
	}
	if fdCount != len(fds) {
		closeAllInts(fds)
		return nil, Metadata{}, fmt.Errorf("tapfd: payload fd=%d but received %d fds", fdCount, len(fds))
	}
	return fdFiles(), meta, nil
}

// parsePayload parses the metadata line up to the first NUL (§4.3), returning
// the recognized fields and the required fd= count. Unknown keys are ignored.
func parsePayload(b []byte) (Metadata, int, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	var m Metadata
	fdCount := 0
	haveFD := false
	for _, tok := range strings.Fields(string(b)) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return Metadata{}, 0, fmt.Errorf("tapfd: token %q has no '='", tok)
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
				return Metadata{}, 0, fmt.Errorf("tapfd: invalid fd=%q", v)
			}
			fdCount, haveFD = c, true
		}
	}
	if !haveFD {
		return Metadata{}, 0, errors.New("tapfd: payload missing required fd= count")
	}
	return m, fdCount, nil
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
