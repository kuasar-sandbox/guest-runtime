// Package proto defines the wire protocol for the sandbox-ctl ↔
// sandbox-init control channel that runs over virtio-vsock.
//
// The channel is bidirectional and short-lived (one request + one
// response per connection, then close). Both directions reuse port
// 5000:
//
//   - guest → host: sandbox-init dial(CID=2, port=5000); CH hybrid
//     vsock proxies to "<vsock-base>_5000" UDS that sandbox-ctl
//     listens on.
//   - host → guest: sandbox-ctl dial(<vsock-base>) UDS, write
//     "CONNECT 5000\n" first; CH proxies the rest to guest port 5000
//     listener that sandbox-init runs.
//
// Wire format on every connection: [4 bytes LE length] [JSON payload].
//
// This package is dependency-light (stdlib only) so the guest
// sandbox-init binary can import it without dragging in YAML or other
// heavy deps.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// VsockHostCID is the well-known guest-side address of the host (always 2).
const VsockHostCID = 2

// VsockGuestCID is the CID we assign to our guest VM. Any value > 2 works;
// 3 is conventional for single-VM-per-host setups.
const VsockGuestCID = 3

// LaunchPort is the vsock port both directions use. sandbox-init dials
// host:LaunchPort for the cold-start hello/launch handshake and for
// app_started/app_exited notifications; sandbox-init also listens on
// guest:LaunchPort so sandbox-ctl can push ping/restore/quiesce.
const LaunchPort = 5000

// MaxMessageBytes bounds the JSON payload size for one message.
const MaxMessageBytes = 64 * 1024

// HostConnectLine is the ASCII prefix sandbox-ctl writes as the first
// bytes of a host→guest connection, telling CH's hybrid vsock proxy
// which guest port to connect to. CH consumes this line and forwards
// everything after it to the guest listener.
//
// Trailing "\n" is required.
var HostConnectLine = []byte(fmt.Sprintf("CONNECT %d\n", LaunchPort))

// LaunchSpec is the payload sandbox-ctl pushes to sandbox-init telling
// it how to start the user application after rootfs is assembled.
//
// Fields mirror the merged result of:
//   - the OCI image config.json appended to boot.root.base (defaults)
//   - the sandbox.yaml `launch:` section (overrides)
//
// Network (optional) carries the IP-layer config for sandbox-init's
// netlink-based applyNetwork pass. nil → no network configuration.
type LaunchSpec struct {
	Exec    string            `json:"exec"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Restart string            `json:"restart,omitempty"` // never|on-failure|always
	Network *NetworkSpec      `json:"network,omitempty"`
}

// NetworkSpec is the resolved guest IP-layer config sandbox-init applies
// via netlink before forking the user app. Empty fields fall back to
// "skip that step" (e.g. empty Gateway → no default route).
type NetworkSpec struct {
	Interface string `json:"interface,omitempty"`
	IPCIDR    string `json:"ip_cidr,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
}

// Message is the typed envelope. Only fields relevant to Type are populated.
type Message struct {
	Type string `json:"type"`

	// hello: guest → host first message; phase carries an optional hint
	// (e.g. "ready"). v1 just uses presence of the message.
	Phase string `json:"phase,omitempty"`

	// launch: host → guest immediately after hello.
	Launch *LaunchSpec `json:"launch,omitempty"`

	// app_started: guest → host after fork/exec succeeds.
	// launch_ack: guest → host after launch spec received and network applied,
	// before forking the user app. Marks the end of the boot/launch transient
	// so host can engage post-boot enforcement (memory.high, controller RPCs).
	PID int `json:"pid,omitempty"`

	// app_exited: guest → host before reboot.
	Code int `json:"code,omitempty"`

	// ping/pong: monotonic id host-assigned; host clock t_send_ns
	// echoed back in pong so host computes RTT without time-sync.
	ID       uint64 `json:"id,omitempty"`
	TSendNs  int64  `json:"t_send_ns,omitempty"`

	// restore: incremented on each restore. Lets guest distinguish
	// "fresh wake" from a duplicate restore message in flight.
	Epoch uint32 `json:"epoch,omitempty"`

	// error: human-readable reason on rejection paths.
	Msg string `json:"msg,omitempty"`
}

// Message type constants. See docs/sandbox-runtime.md §4.3 for the
// directions and configured-response pairs.
const (
	TypeHello      = "hello"
	TypeLaunch     = "launch"
	TypeAppStarted = "app_started"
	TypeAppExited  = "app_exited"
	TypeLaunchAck  = "launch_ack"
	TypePing       = "ping"
	TypePong       = "pong"
	TypeRestore    = "restore"
	TypeRestored   = "restored"
	TypeQuiesce    = "quiesce"
	TypeQuiesced   = "quiesced"
	TypeAck        = "ack"
	TypeError      = "error"
)

// Default per-message deadlines (§9.1.6). Callers pass these to
// SetDeadline on the underlying conn covering dial+write+read.
const (
	DeadlineAppNotify = 200 * time.Millisecond // app_started / app_exited / launch_ack
	DeadlinePing      = 200 * time.Millisecond
	DeadlineQuiesce   = 5 * time.Second
	DeadlineRestore   = 5 * time.Second
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
