package snapshot

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
// down so the cloud-hypervisor process exits. Used by the "destroy"
// snapshot path (resume_after=false): after the bundle is written the
// guest is left paused, and this is what makes `sandbox-ctl run` return.
func CHShutdownVMM(apiSock string) error {
	return chAPI(apiSock, "PUT", "/api/v1/vmm.shutdown", "")
}

// chAPI sends a tiny HTTP/1.1 request over the CH api UDS. CH speaks
// HTTP/1.1; we hand-roll because the call rate is one-shot per
// snapshot operation.
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
