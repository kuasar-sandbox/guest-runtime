package tapfd

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParsePayload(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantMAC string
		wantIP  string
		wantMTU int
		wantFD  int
		wantErr string
	}{
		{name: "full", in: "mac=02:00:00:00:80:01 mtu=1500 ip=169.254.1.1 fd=1\x00",
			wantMAC: "02:00:00:00:80:01", wantIP: "169.254.1.1", wantMTU: 1500, wantFD: 1},
		{name: "ignores unknown keys + trailing bytes", in: "port=3 mac=aa:bb:cc:dd:ee:ff fd=2\x00garbage after nul",
			wantMAC: "aa:bb:cc:dd:ee:ff", wantFD: 2},
		{name: "missing fd", in: "mac=x ip=y\x00", wantErr: "missing required fd"},
		{name: "bad fd", in: "fd=zero\x00", wantErr: "invalid fd"},
		{name: "zero fd", in: "fd=0\x00", wantErr: "invalid fd"},
		{name: "token without =", in: "mac=x bogus fd=1\x00", wantErr: "no '='"},
		{name: "value may contain =", in: "k=a=b fd=1\x00", wantFD: 1}, // first = splits; rest kept (unknown key)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta, fdCount, err := parsePayload([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if meta.MAC != tc.wantMAC || meta.IP != tc.wantIP || meta.MTU != tc.wantMTU || fdCount != tc.wantFD {
				t.Fatalf("got mac=%q ip=%q mtu=%d fd=%d", meta.MAC, meta.IP, meta.MTU, fdCount)
			}
		})
	}
}

// socketPair returns two connected *net.UnixConn (a sender, b receiver).
func socketPair(t *testing.T) (a, b *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	mk := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "sp")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("fileconn: %v", err)
		}
		return c.(*net.UnixConn)
	}
	return mk(fds[0]), mk(fds[1])
}

func sendMsg(t *testing.T, c *net.UnixConn, payload string, fds ...int) {
	t.Helper()
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	if _, _, err := c.WriteMsgUnix([]byte(payload), oob, nil); err != nil {
		t.Fatalf("writemsg: %v", err)
	}
}

func TestRecvFd_OK(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer dn.Close()

	sendMsg(t, a, "mac=02:00:00:00:80:01 mtu=1450 ip=169.254.4.1 fd=1\x00", int(dn.Fd()))
	files, meta, err := RecvFd(b)
	if err != nil {
		t.Fatalf("RecvFd: %v", err)
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if meta.MAC != "02:00:00:00:80:01" || meta.IP != "169.254.4.1" || meta.MTU != 1450 {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestRecvFd_CountMismatch(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer dn.Close()

	sendMsg(t, a, "fd=2\x00", int(dn.Fd())) // declares 2, sends 1
	files, _, err := RecvFd(b)
	if err == nil || !strings.Contains(err.Error(), "fd=2") {
		for _, f := range files {
			f.Close()
		}
		t.Fatalf("want fd-count mismatch error, got %v", err)
	}
}

func TestAcquire_Errors(t *testing.T) {
	if _, _, err := Acquire(context.Background(), nil, time.Second); err == nil {
		t.Fatal("empty argv: want error")
	}
	// Helper exits non-zero without handing over a fd (§5.1: must fail).
	if _, _, err := Acquire(context.Background(), []string{"sh", "-c", "exit 3"}, 2*time.Second); err == nil {
		t.Fatal("non-zero helper: want error")
	}
}
