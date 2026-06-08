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
		network string
		addr    string
		accept  bool
	}{
		// dial mode, tcp target (LOCAL:host:port)
		{"/run/envd.sock:127.0.0.1:49983", "/run/envd.sock", 0, "tcp", "127.0.0.1:49983", false},
		{"fd=3:127.0.0.1:49983", "", 3, "tcp", "127.0.0.1:49983", false},
		{"@envd:[::1]:8080", "@envd", 0, "tcp", "[::1]:8080", false},
		{"fd=7:localhost:80", "", 7, "tcp", "localhost:80", false},
		{"./rel.sock:10.0.0.1:5432", "./rel.sock", 0, "tcp", "10.0.0.1:5432", false},
		// dial mode, unix target (①): '/' or '@' prefix ⇒ unix
		{"/run/db.sock:/var/run/pg.sock", "/run/db.sock", 0, "unix", "/var/run/pg.sock", false},
		{"/run/db.sock:@pg", "/run/db.sock", 0, "unix", "@pg", false},
		// accept mode, tcp target (②): LOCAL::host:port
		{"/run/api.sock::0.0.0.0:8080", "/run/api.sock", 0, "tcp", "0.0.0.0:8080", true},
		{"/run/api.sock::[::1]:8080", "/run/api.sock", 0, "tcp", "[::1]:8080", true},
		{"fd=3::0.0.0.0:8080", "", 3, "tcp", "0.0.0.0:8080", true}, // fd= local is valid in accept mode
		// accept mode, unix target (③): LOCAL::/path or ::@abstract
		{"/run/api.sock::/run/up.sock", "/run/api.sock", 0, "unix", "/run/up.sock", true},
		{"@api::@up", "@api", 0, "unix", "@up", true},
	}
	for _, tc := range ok {
		got, err := ParseForwardSpec(tc.in)
		if err != nil {
			t.Errorf("ParseForwardSpec(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.UDSPath != tc.udsPath || got.ListenFD != tc.fd || got.Network != tc.network ||
			got.Address != tc.addr || got.Accept != tc.accept {
			t.Errorf("ParseForwardSpec(%q) = {uds:%q fd:%d net:%q addr:%q accept:%v}, want {uds:%q fd:%d net:%q addr:%q accept:%v}",
				tc.in, got.UDSPath, got.ListenFD, got.Network, got.Address, got.Accept,
				tc.udsPath, tc.fd, tc.network, tc.addr, tc.accept)
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
		"/p:",                   // empty target (dial)
		"/p::",                  // empty target (accept)
		"/p:rel/sock",           // relative unix target (not '/' or '@') parses as bad host:port
		"/p::rel.sock",          // relative unix target in accept mode
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
