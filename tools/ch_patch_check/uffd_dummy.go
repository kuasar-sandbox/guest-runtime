package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// runUffdDummy probes commit external uffd handler: CH performs the va_report handshake
// against an external uffd handler and registers the inherited uffd.
//
// Method:
//  1. memfd_create + ftruncate + mmap (parent VMA — gives us page-zeroing
//     authority via the same uffd later)
//  2. userfaultfd() + UFFDIO_API(MISSING_SHMEM | EVENT_REMOVE)
//     (UFFDIO_REGISTER on the parent VMA is left to production code; this
//     probe is only checking CH's child-side register path)
//  3. Listen on a UDS, accept one connection, read va_report message,
//     reply "ack". Record the reported VA + size.
//  4. Run a UFFD reader goroutine that handles MISSING faults from the
//     CH-side VMA by UFFDIO_COPY of zeroed pages — so CH's first memory
//     accesses don't deadlock.
//  5. Spawn CH with --memory-zone fd=3,uffd_fd=4,uffd_socket=...
//  6. Wait for the va_report → ack → "RAM region mapping at 0x" path
//     to complete. SIGTERM CH.
func runUffdDummy(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("uffd-dummy", flag.ContinueOnError)
	var cf commonFlags
	cf.bind(fs, 60)
	memMB := fs.Int("mem-mb", 256, "RAM size in MiB")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	size := int64(*memMB) << 20

	// (1) memfd
	memfd, err := unix.MemfdCreate("ch-uffd-dummy", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(memfd)
	if err := unix.Ftruncate(memfd, size); err != nil {
		return fmt.Errorf("ftruncate: %w", err)
	}
	memfdFile := os.NewFile(uintptr(memfd), "memfd")
	defer memfdFile.Close()

	// (2) uffd
	uffd, _, errno := syscall.Syscall(unix.SYS_USERFAULTFD,
		uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK), 0, 0)
	if errno != 0 {
		return fmt.Errorf("userfaultfd: %v", errno)
	}
	uffdInt := int(uffd)
	defer unix.Close(uffdInt)
	if err := uffdAPI(uffdInt); err != nil {
		return fmt.Errorf("UFFDIO_API: %w", err)
	}
	uffdFile := os.NewFile(uffd, "uffd")
	defer uffdFile.Close()

	// (3) UDS listener for va_report
	runDir, err := pickRunDir("uffd-dummy")
	if err != nil {
		return err
	}
	if !cf.keep {
		defer os.RemoveAll(runDir)
	}
	uffdSock := filepath.Join(runDir, "uffd.sock")
	listener, err := net.Listen("unix", uffdSock)
	if err != nil {
		return fmt.Errorf("listen uds: %w", err)
	}
	defer listener.Close()

	// Accept va_report → reply ack, in goroutine
	gotReport := make(chan vaReport, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		req, err := readLPJSON(c)
		if err != nil {
			fmt.Fprintf(stderrSink, "uds read: %v\n", err)
			return
		}
		fmt.Fprintf(stderrSink, "==> received va_report: %+v\n", req)
		gotReport <- req
		// reply ack
		ack := []byte(`{"type":"ack"}`)
		if err := writeLPJSON(c, ack); err != nil {
			fmt.Fprintf(stderrSink, "uds write: %v\n", err)
		}
	}()

	// (5) spawn CH
	mon := newMonitorReader("ch", "RAM region mapping at 0x")
	args := []string{
		"--kernel", cf.vmlinux,
		"--memory", "size=0",
		"--memory-zone",
		fmt.Sprintf("id=z0,size=%dM,shared=on,fd=3,uffd_fd=4,uffd_socket=%s",
			*memMB, uffdSock),
		"--cpus", "boot=1",
		"--serial", "tty",
		"--console", "off",
		"-v",
	}
	subCtx, cancel := context.WithTimeout(ctx, time.Duration(cf.timeout)*time.Second)
	defer cancel()
	cmd, err := startCH(subCtx, cf, mon, args, []*os.File{memfdFile, uffdFile})
	if err != nil {
		return fmt.Errorf("spawn CH: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	// (6) verify report received + RAM region marker fired
	select {
	case r := <-gotReport:
		if r.Size != uint64(size) {
			return fmt.Errorf("va_report size mismatch: got %d want %d", r.Size, size)
		}
		if r.VAStart == 0 {
			return fmt.Errorf("va_report va_start is zero")
		}
		fmt.Fprintf(stderrSink, "==> va_report ok: zone=%s va=%#x size=%d\n",
			r.ZoneID, r.VAStart, r.Size)
	case <-subCtx.Done():
		return fmt.Errorf("timeout waiting for va_report")
	}

	select {
	case <-mon.hit:
		fmt.Fprintln(stderrSink, "==> RAM region mapped after va_report+register")
	case <-subCtx.Done():
		return fmt.Errorf("timeout waiting for RAM-region marker")
	}

	return nil
}

type vaReport struct {
	Type    string `json:"type"`
	ZoneID  string `json:"zone_id"`
	VAStart uint64 `json:"va_start"`
	Size    uint64 `json:"size"`
}

func readLPJSON(c net.Conn) (vaReport, error) {
	var lenBuf [4]byte
	if _, err := readFull(c, lenBuf[:]); err != nil {
		return vaReport{}, err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n > 64*1024 {
		return vaReport{}, fmt.Errorf("oversized message: %d", n)
	}
	body := make([]byte, n)
	if _, err := readFull(c, body); err != nil {
		return vaReport{}, err
	}
	var r vaReport
	if err := json.Unmarshal(body, &r); err != nil {
		return vaReport{}, fmt.Errorf("json: %w (body=%s)", err, body)
	}
	if r.Type != "va_report" {
		return vaReport{}, fmt.Errorf("unexpected type %q", r.Type)
	}
	return r, nil
}

func readFull(c net.Conn, buf []byte) (int, error) {
	off := 0
	for off < len(buf) {
		n, err := c.Read(buf[off:])
		off += n
		if err != nil {
			return off, err
		}
	}
	return off, nil
}

func writeLPJSON(c net.Conn, body []byte) error {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := c.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := c.Write(body)
	return err
}

// uffdAPI sends UFFDIO_API to enable EVENT_REMOVE | MISSING_SHMEM. The
// child (CH) doesn't repeat UFFDIO_API on its inherited uffd — features
// stay set across fork/exec.
func uffdAPI(uffd int) error {
	const UFFDIO_API uintptr = 0xc018_aa3f
	const UFFD_API uint64 = 0xaa
	type uffdioAPI struct {
		API      uint64
		Features uint64
		Ioctls   uint64
	}
	const (
		UFFD_FEATURE_MISSING_SHMEM uint64 = 1 << 1
		UFFD_FEATURE_EVENT_REMOVE  uint64 = 1 << 9
	)
	req := uffdioAPI{
		API:      UFFD_API,
		Features: UFFD_FEATURE_MISSING_SHMEM | UFFD_FEATURE_EVENT_REMOVE,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(uffd), UFFDIO_API,
		uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("ioctl UFFDIO_API: %v", errno)
	}
	return nil
}
