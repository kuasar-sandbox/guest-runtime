package sandbox

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestParseForwardSpec(t *testing.T) {
	ok := []struct {
		in      string
		udsPath string
		fd      int
		addr    string
	}{
		{"/run/envd.sock:127.0.0.1:49983", "/run/envd.sock", 0, "127.0.0.1:49983"},
		{"fd=3:127.0.0.1:49983", "", 3, "127.0.0.1:49983"},
		{"@envd:[::1]:8080", "@envd", 0, "[::1]:8080"},
		{"fd=7:localhost:80", "", 7, "localhost:80"},
		{"./rel.sock:10.0.0.1:5432", "./rel.sock", 0, "10.0.0.1:5432"},
	}
	for _, tc := range ok {
		got, err := ParseForwardSpec(tc.in)
		if err != nil {
			t.Errorf("ParseForwardSpec(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.UDSPath != tc.udsPath || got.ListenFD != tc.fd || got.Address != tc.addr {
			t.Errorf("ParseForwardSpec(%q) = {uds:%q fd:%d addr:%q}, want {uds:%q fd:%d addr:%q}",
				tc.in, got.UDSPath, got.ListenFD, got.Address, tc.udsPath, tc.fd, tc.addr)
		}
		if got.Network != "tcp" {
			t.Errorf("ParseForwardSpec(%q): network %q, want tcp", tc.in, got.Network)
		}
	}

	bad := []string{
		"",                      // empty
		"nocolons",              // no colon at all
		"/p:127.0.0.1",          // target missing port
		"/p:127.0.0.1:",         // empty port
		"/p:127.0.0.1:notaport", // non-numeric port
		":127.0.0.1:80",         // empty local endpoint
		"fd=0:127.0.0.1:80",     // fd must be > 0
		"fd=x:127.0.0.1:80",     // non-numeric fd
		"fd=-1:127.0.0.1:80",    // negative fd
	}
	for _, in := range bad {
		if _, err := ParseForwardSpec(in); err == nil {
			t.Errorf("ParseForwardSpec(%q): expected error, got nil", in)
		}
	}
}

// TestForwarderAcceptAndClose drives the host accept path: a UDS listener
// accepts a local connection, serve() tries to reach the (absent) guest
// via a bogus vsock base and therefore closes the local conn; after Close
// the listener is gone.
func TestForwarderAcceptAndClose(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "fwd.sock")
	spec, err := ParseForwardSpec(udsPath + ":127.0.0.1:49983")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	f := NewForwarder(filepath.Join(dir, "nonexistent-vsock.sock"), func(string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx, []ForwardSpec{spec}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c, err := net.DialTimeout("unix", udsPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	// serve() can't reach the guest (bogus vsock base) → it closes the conn.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Error("expected local conn to be closed by serve (guest unreachable)")
	}
	_ = c.Close()

	f.Close()
	if _, err := net.DialTimeout("unix", udsPath, 500*time.Millisecond); err == nil {
		t.Error("expected dial to fail after Close (listener removed)")
	}
}
