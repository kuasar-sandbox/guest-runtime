// Package proto defines the wire protocol for the sandbox-ctl ↔
// sandbox-init control channel that runs over virtio-vsock.
//
// The host's sandbox-ctl listens on a UDS that cloud-hypervisor's hybrid
// vsock proxies to/from guest CID=2 (host) port=LaunchPort. The guest's
// sandbox-init opens a vsock socket (AF_VSOCK) to (CID=2, port=LaunchPort)
// and exchanges JSON-over-length-prefix messages.
//
// This package is dependency-light (stdlib only) so the guest sandbox-init
// binary can import it without dragging in YAML or other heavy deps.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// VsockHostCID is the well-known guest-side address of the host (always 2).
const VsockHostCID = 2

// VsockGuestCID is the CID we assign to our guest VM. Any value > 2 works;
// 3 is conventional for single-VM-per-host setups.
const VsockGuestCID = 3

// LaunchPort is the vsock port sandbox-init connects to for receiving the
// launch spec from sandbox-ctl. Hard-coded so guest doesn't need extra
// kernel cmdline parameters.
const LaunchPort = 5000

// MaxMessageBytes bounds the JSON payload size for one message.
const MaxMessageBytes = 64 * 1024

// LaunchSpec is the payload sandbox-ctl pushes to sandbox-init telling it
// how to start the user application after rootfs is assembled.
//
// The fields mirror the merged result of:
//   - the OCI image config.json appended to boot.root.base (defaults)
//   - the sandbox.yaml `launch:` section (overrides)
//
// Network (optional) carries the IP-layer config for sandbox-init's
// netlink-based applyNetwork pass — replaces the kernel `ip=...`
// cmdline + CONFIG_IP_PNP path. nil → no network configuration.
type LaunchSpec struct {
	Exec    string            `json:"exec"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Restart string            `json:"restart,omitempty"` // never|on-failure|always
	Network *NetworkSpec      `json:"network,omitempty"`
}

// NetworkSpec is the resolved guest IP-layer config sandbox-init applies
// via netlink before forking the user app. All fields optional — empty
// fields fall back to "skip that step" (e.g. empty Gateway → no default
// route added).
type NetworkSpec struct {
	Interface string `json:"interface,omitempty"` // guest iface (default "eth0")
	IPCIDR    string `json:"ip_cidr,omitempty"`   // e.g. "169.254.1.1/31"; IPv4 or IPv6
	Gateway   string `json:"gateway,omitempty"`   // default route next-hop
	Hostname  string `json:"hostname,omitempty"`  // sethostname target
}

// Message is the typed envelope for messages on the launch channel. Only
// fields relevant to the message Type are populated.
type Message struct {
	Type string `json:"type"`

	// hello: guest → host first message; phase carries an optional hint
	// (e.g. "ready"). v1 just uses presence of the message.
	Phase string `json:"phase,omitempty"`

	// launch: host → guest immediately after hello.
	Launch *LaunchSpec `json:"launch,omitempty"`

	// app_started: guest → host after fork/exec succeeds.
	PID int `json:"pid,omitempty"`

	// app_exited: guest → host before reboot.
	Code int `json:"code,omitempty"`
}

// Message type constants.
const (
	TypeHello      = "hello"
	TypeLaunch     = "launch"
	TypeAppStarted = "app_started"
	TypeAppExited  = "app_exited"
)

// WriteMessage writes one message in length-prefix-JSON wire format:
//
//	[4 bytes LE length] [JSON payload]
func WriteMessage(w io.Writer, m *Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("proto: marshal: %w", err)
	}
	if len(payload) > MaxMessageBytes {
		return fmt.Errorf("proto: payload %d > max %d", len(payload), MaxMessageBytes)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("proto: write header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("proto: write payload: %w", err)
	}
	return nil
}

// ReadMessage reads exactly one message.
func ReadMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("proto: zero-length payload")
	}
	if n > MaxMessageBytes {
		return nil, fmt.Errorf("proto: payload %d > max %d", n, MaxMessageBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("proto: read payload: %w", err)
	}
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("proto: unmarshal: %w", err)
	}
	return &m, nil
}
