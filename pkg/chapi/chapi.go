// Package chapi provides a tiny HTTP/1.1 client over the cloud-hypervisor
// api-socket (Unix Domain Socket). It is intentionally a leaf package
// (only stdlib deps) so both pkg/sandbox (lifecycle, balloon) and
// pkg/sandbox/snapshot can import it without creating a cycle.
//
// CH speaks plain HTTP/1.1 on the UDS exposed by --api-socket. The call
// rate is low (one-shot per snapshot/shutdown), so a hand-rolled
// request/response keeps the dep surface minimal compared to net/http.
package chapi

import (
	"fmt"
	"net"
	"time"
)

// CHPause issues PUT /api/v1/vm.pause to the CH api socket.
func CHPause(apiSock string) error {
	return chAPI(apiSock, "PUT", "/api/v1/vm.pause", "")
}

// CHResume issues PUT /api/v1/vm.resume.
func CHResume(apiSock string) error {
	return chAPI(apiSock, "PUT", "/api/v1/vm.resume", "")
}

// CHSnapshot issues PUT /api/v1/vm.snapshot with destination_url=
// file://<dir>. CH writes config.json + state.json there. With our
// patches, memory-ranges is skipped for fd-backed user-managed zones.
func CHSnapshot(apiSock, destURL string) error {
	body := fmt.Sprintf(`{"destination_url":"%s"}`, destURL)
	return chAPI(apiSock, "PUT", "/api/v1/vm.snapshot", body)
}

// CHShutdownVMM issues PUT /api/v1/vmm.shutdown — tears the whole VMM
// down so the cloud-hypervisor process exits. CH performs an ordered
// internal cleanup (stop vCPU → destroy devices → release memory zones
// → close sockets → exit). Preferred over forwarding host SIGTERM,
// which forces an unordered signal-handler exit and leaves Linux to
// unmap large memory zones via the reaper path (slow under host
// oversubscribe — see density-perf forensics).
//
// Used by both the "destroy" snapshot path (snapshot.go --resume=false)
// and by lifecycle.go's normal shutdown path.
func CHShutdownVMM(apiSock string) error {
	return chAPI(apiSock, "PUT", "/api/v1/vmm.shutdown", "")
}

// chAPI sends a tiny HTTP/1.1 request over the CH api UDS.
func chAPI(sock, method, path, body string) error {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("ch api dial %s: %w", sock, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := c.Write([]byte(req)); err != nil {
		return fmt.Errorf("ch api write: %w", err)
	}
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	resp := string(buf[:n])
	if len(resp) < 12 {
		return fmt.Errorf("ch api short response: %q", resp)
	}
	status := resp[9:12]
	if status[0] != '2' {
		return fmt.Errorf("ch api %s %s non-2xx: %q", method, path, resp)
	}
	return nil
}
