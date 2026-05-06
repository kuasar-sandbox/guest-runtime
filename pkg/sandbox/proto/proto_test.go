package proto

import (
	"bytes"
	"reflect"
	"testing"
)

func TestRoundTrip_Launch(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec:    "/usr/bin/foo",
			Args:    []string{"--flag", "value with spaces", "comma,inside,arg"},
			Env:     map[string]string{"PATH": "/bin:/usr/bin", "HOME": "/root"},
			Workdir: "/var/data",
			Restart: "on-failure",
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
}

func TestRoundTrip_Hello(t *testing.T) {
	m := &Message{Type: TypeHello, Phase: "ready"}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadMessage(&buf)
	if got.Type != TypeHello || got.Phase != "ready" {
		t.Errorf("got %+v", got)
	}
}

func TestRoundTrip_LaunchWithNetwork(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec: "/usr/bin/foo",
			Network: &NetworkSpec{
				Interface: "eth0",
				IPCIDR:    "169.254.1.1/31",
				Gateway:   "169.254.1.0",
				Hostname:  "test-sandbox",
			},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Launch.Network, m.Launch.Network) {
		t.Errorf("Network mismatch:\n got=%+v\nwant=%+v", got.Launch.Network, m.Launch.Network)
	}
}

func TestRoundTrip_LaunchWithoutNetwork(t *testing.T) {
	// nil Network omitempty — backward compatible with old sandbox-init.
	m := &Message{
		Type:   TypeLaunch,
		Launch: &LaunchSpec{Exec: "/usr/bin/foo"},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadMessage(&buf)
	if got.Launch.Network != nil {
		t.Errorf("Network should be nil when omitted, got %+v", got.Launch.Network)
	}
}

func TestRead_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	// header says 1 MiB, exceeds MaxMessageBytes
	buf.Write([]byte{0, 0, 0x10, 0})
	_, err := ReadMessage(&buf)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

func TestRead_TruncatedHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x10, 0x00}) // only 2 bytes
	_, err := ReadMessage(&buf)
	if err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestWrite_TooLarge(t *testing.T) {
	big := make([]byte, MaxMessageBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	m := &Message{Type: TypeHello, Phase: string(big)}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err == nil {
		t.Fatal("expected error for oversized payload")
	}
}
