package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// runSnapshotSkip probes commit snapshot-skip-user-managed: snapshot of an fd-backed zone
// must not produce a memory-ranges file.
//
// Method:
//  1. memfd-backed CH boot (same as memfd-boot, but with --api-socket
//     so we can talk to /vm.snapshot)
//  2. wait for "RAM region mapping at" marker (boot reached memory init)
//  3. POST /api/v1/vm.pause
//  4. PUT /api/v1/vm.snapshot {destination_url=file://<tmpdir>}
//  5. assert <tmpdir>/config.json exists, <tmpdir>/state.json exists,
//     <tmpdir>/memory-ranges does NOT exist
func runSnapshotSkip(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("snapshot-skip", flag.ContinueOnError)
	var cf commonFlags
	cf.bind(fs, 60)
	memMB := fs.Int("mem-mb", 256, "RAM size in MiB")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	size := int64(*memMB) << 20

	// memfd setup
	memfd, err := unix.MemfdCreate("ch-snap-skip", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(memfd)
	if err := unix.Ftruncate(memfd, size); err != nil {
		return fmt.Errorf("ftruncate: %w", err)
	}
	memfdFile := os.NewFile(uintptr(memfd), "memfd")
	defer memfdFile.Close()

	runDir, err := pickRunDir("snapshot-skip")
	if err != nil {
		return err
	}
	if !cf.keep {
		defer os.RemoveAll(runDir)
	}
	apiSock := filepath.Join(runDir, "ch.sock")
	snapDir := filepath.Join(runDir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}

	mon := newMonitorReader("ch", "RAM region mapping at 0x")
	args := []string{
		"--api-socket", apiSock,
		"--kernel", cf.vmlinux,
		"--memory", "size=0",
		"--memory-zone",
		fmt.Sprintf("id=z0,size=%dM,shared=on,fd=3", *memMB),
		"--cpus", "boot=1",
		"--serial", "tty",
		"--console", "off",
		"-v",
	}
	subCtx, cancel := context.WithTimeout(ctx, time.Duration(cf.timeout)*time.Second)
	defer cancel()
	cmd, err := startCH(subCtx, cf, mon, args, []*os.File{memfdFile})
	if err != nil {
		return fmt.Errorf("spawn CH: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	// Wait for boot to reach memory mapping.
	select {
	case <-mon.hit:
	case <-subCtx.Done():
		return fmt.Errorf("timeout waiting for RAM-region marker")
	}
	// Give CH a moment to come up + start its API server.
	if err := waitForAPI(apiSock, 5*time.Second); err != nil {
		return fmt.Errorf("waiting for api: %w", err)
	}

	// Pause + snapshot.
	if err := chAPI(apiSock, "PUT", "/api/v1/vm.pause", ""); err != nil {
		return fmt.Errorf("vm.pause: %w", err)
	}
	dest := fmt.Sprintf(`{"destination_url":"file://%s"}`, snapDir)
	if err := chAPI(apiSock, "PUT", "/api/v1/vm.snapshot", dest); err != nil {
		return fmt.Errorf("vm.snapshot: %w", err)
	}

	// Verify outputs.
	mustExist := []string{"config.json", "state.json"}
	mustNotExist := []string{"memory-ranges"}
	for _, f := range mustExist {
		p := filepath.Join(snapDir, f)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("expected %s missing: %w", f, err)
		}
		fmt.Fprintf(stderrSink, "==> ok: %s present\n", f)
	}
	for _, f := range mustNotExist {
		p := filepath.Join(snapDir, f)
		if st, err := os.Stat(p); err == nil {
			return fmt.Errorf("memory-ranges should not exist for fd-backed zone, got size %d", st.Size())
		}
		fmt.Fprintf(stderrSink, "==> ok: %s absent (patch snapshot-skip-user-managed effective)\n", f)
	}

	return nil
}

// waitForAPI polls the api UDS until it accepts a connection.
func waitForAPI(sock string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("api socket %s not ready in %s", sock, deadline)
}

// chAPI sends an HTTP request to CH's UDS-based REST API. CH speaks
// HTTP/1.1 over the UDS; we hand-roll a tiny client to avoid pulling
// in a full HTTP-over-UDS dance.
func chAPI(sock, method, path, body string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := c.Write([]byte(req)); err != nil {
		return err
	}

	// Read full response; expect 2xx.
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	resp := string(buf[:n])
	// First status line e.g. "HTTP/1.1 204 No Content"
	if len(resp) < 12 {
		return fmt.Errorf("short response: %q", resp)
	}
	status := resp[9:12]
	if status[0] != '2' {
		return fmt.Errorf("non-2xx response: %q", resp)
	}
	return nil
}
