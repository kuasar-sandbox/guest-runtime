// Package proto defines the wire protocol for the sandbox-ctl ↔
// sandbox-init control channel that runs over virtio-vsock, plus the
// constants shared with the stdio MUX sub-protocol (pkg/sandbox/mux).
//
// Two kinds of connections (docs/sandbox-runtime.md §4):
//
//   (1) Management short connections — one request + one response per
//       connection, then close. Both directions reuse port 5000:
//         - guest → host: sandbox-init dial(CID=2, port=5000); CH hybrid
//           vsock proxies to "<vsock-base>_5000" UDS that sandbox-ctl
//           listens on. Carries: hello/launch handshake, app_started,
//           app_exited, mem_report.
//         - host → guest: sandbox-ctl dial(<vsock-base>) UDS, write
//           "CONNECT 5000\n" first; CH proxies the rest to guest port
//           5000 listener. Carries: ping, quiesce, restore, attach.
//       Wire: [4 bytes LE length][JSON Message].
//
//   (2) The MUX long connection — at most one. The launch / restore /
//       attach operations DON'T close their connection after the *_ack:
//       it stays open and switches to the framed MUX sub-protocol
//       (pkg/sandbox/mux) which carries the app's stdin/stdout/stderr
//       (or a pty). See StdioSpec for what's negotiated in the *_ack.
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
// app_started/app_exited/mem_report notifications; sandbox-init also
// listens on guest:LaunchPort so sandbox-ctl can push ping/restore/
// quiesce/attach.
const LaunchPort = 5000

// MaxMessageBytes bounds the JSON payload size for one management message.
const MaxMessageBytes = 64 * 1024

// HostConnectLine is the ASCII prefix sandbox-ctl writes as the first
// bytes of a host→guest connection, telling CH's hybrid vsock proxy
// which guest port to connect to. CH consumes this line and forwards
// everything after it to the guest listener. CH then writes back an
// "OK <local_port>\n" line which the caller must drain before reading
// any protocol payload. Trailing "\n" is required.
var HostConnectLine = []byte(fmt.Sprintf("CONNECT %d\n", LaunchPort))

// --- LaunchSpec ------------------------------------------------------

// LaunchSpec is the payload sandbox-ctl pushes to sandbox-init telling
// it how to start the user application after rootfs is assembled.
//
// Fields mirror the merged result of:
//   - the OCI image config.json appended to boot.root.base (defaults)
//   - the sandbox.yaml `launch:` section (overrides)
//
// Network (optional) carries the IP-layer config for sandbox-init's
// netlink-based applyNetwork pass. nil → no network configuration.
// Stdio carries the app stdio mode the host wants (see StdioSpec).
type LaunchSpec struct {
	Exec    string            `json:"exec"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Restart string            `json:"restart,omitempty"` // never|on-failure|always
	Network *NetworkSpec      `json:"network,omitempty"`
	Stdio   StdioSpec         `json:"stdio,omitempty"`
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

// StdioSpec describes the app's stdin/stdout/stderr wiring. It appears
// in LaunchSpec (what the host wants) and is echoed back — possibly
// narrowed — in launch_ack / restore_ack / attach_ack (what the guest
// actually set up). It maps directly onto the MUX stream set
// (docs/sandbox-runtime.md §3.5 / §4.5):
//
//   - TTY == true  → pty mode: the app gets a real pty (isatty()=true);
//     MUX streams = {control, pty}. stdin/stdout/stderr ignored
//     (stdout+stderr merge onto the pty). Winsize is the initial size.
//   - TTY == false → pipe mode: MUX streams = {control} ∪ {stdin if
//     Stdin, stdout if Stdout, stderr if Stderr}. An app fd with no
//     channel is wired to /dev/null (stdin) by the guest.
type StdioSpec struct {
	TTY     bool     `json:"tty,omitempty"`
	Winsize *Winsize `json:"winsize,omitempty"`
	Stdin   bool     `json:"stdin,omitempty"`
	Stdout  bool     `json:"stdout,omitempty"`
	Stderr  bool     `json:"stderr,omitempty"`
}

// Winsize is a terminal window size (cols × rows).
type Winsize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// --- ExecSpec --------------------------------------------------------

// ExecSpec is the payload of an `exec` reverse-channel op: run an
// ad-hoc command inside the already-running sandbox (a sibling of the
// user app, not a replacement). It is independent of LaunchSpec — exec
// sessions are concurrent and short-lived, each with its own stdio MUX.
//
// Argv[0] is the program: resolved via PATH lookup when it has no '/'.
// Env is merged over a default PATH (empty → just the default PATH).
// Cwd empty / "/" → the guest root. Stdio works exactly like
// LaunchSpec.Stdio (pty iff TTY, else the requested pipe subset).
type ExecSpec struct {
	Argv  []string          `json:"argv"`
	Env   map[string]string `json:"env,omitempty"`
	Cwd   string            `json:"cwd,omitempty"`
	Stdio StdioSpec         `json:"stdio,omitempty"`
}

// App lifecycle state reported in restore_ack / attach_ack.
const (
	AppStateRunning = "running"
	AppStateExited  = "exited"
)

// --- Message ---------------------------------------------------------

// Message is the typed envelope for management-connection messages.
// Only fields relevant to Type are populated.
type Message struct {
	Type string `json:"type"`

	// hello: guest → host first message; phase carries an optional hint
	// (e.g. "ready"). v1 just uses presence of the message.
	Phase string `json:"phase,omitempty"`

	// launch: host → guest immediately after hello.
	Launch *LaunchSpec `json:"launch,omitempty"`

	// launch_ack / restore_ack / attach_ack: guest → host. Stdio = the
	// channel set the guest actually established (the host bridges
	// exactly these MUX streams). For restore_ack / attach_ack, AppState
	// reports whether the user app is still running, plus Code/TermSignal
	// if it already exited.
	Stdio    *StdioSpec `json:"stdio,omitempty"`
	AppState string     `json:"app_state,omitempty"`

	// app_started: guest → host after fork/exec succeeds.
	PID int `json:"pid,omitempty"`

	// app_exited: guest → host before reboot. TermSignal != 0 means the
	// app was killed by that signal (Code is then the signal number too,
	// per shell 128+sig convention applied host-side).
	Code       int `json:"code,omitempty"`
	TermSignal int `json:"term_signal,omitempty"`

	// ping/pong: monotonic id host-assigned; host clock t_send_ns
	// echoed back in pong so host computes RTT without time-sync.
	ID      uint64 `json:"id,omitempty"`
	TSendNs int64  `json:"t_send_ns,omitempty"`

	// restore / attach: incremented on each restore/attach. Lets guest
	// distinguish "fresh wake" from a duplicate message in flight.
	Epoch uint32 `json:"epoch,omitempty"`

	// exec: the command + stdio to run inside the running sandbox. The
	// exec / exec_ack handshake mirrors restore / attach — same conn
	// becomes the stdio MUX after exec_ack.
	Exec *ExecSpec `json:"exec,omitempty"`

	// restore: host wall clock (UnixNano) captured just before the
	// notify is sent. A snapshot's CLOCK_REALTIME is reloaded verbatim
	// by CH on restore, so the guest's wall clock is stale by the whole
	// dormant interval; the guest jumps CLOCK_REALTIME to this on
	// restore. Unset (0) on attach — a live VM's clock is fine.
	WallclockNs int64 `json:"wallclock_ns,omitempty"`

	// mem_report: guest → host periodic /proc/meminfo snapshot used by
	// the host-side balloon controller to drive vm.resize.
	MemAvailableBytes uint64 `json:"mem_avail_bytes,omitempty"`
	MemTotalBytes     uint64 `json:"mem_total_bytes,omitempty"`

	// error: human-readable reason on rejection paths.
	Msg string `json:"msg,omitempty"`
}

// Message type constants. See docs/sandbox-runtime.md §4.3 for the
// directions, the request/response pairs, and which connections upgrade
// to the MUX after their *_ack.
const (
	TypeHello      = "hello"
	TypeLaunch     = "launch"
	TypeLaunchAck  = "launch_ack"
	TypeAppStarted = "app_started"
	TypeAppExited  = "app_exited"
	TypePing       = "ping"
	TypePong       = "pong"
	TypeRestore    = "restore"
	TypeRestoreAck = "restore_ack"
	TypeAttach     = "attach"
	TypeAttachAck  = "attach_ack"
	TypeQuiesce    = "quiesce"
	TypeQuiesced   = "quiesced"
	TypeExec       = "exec"
	TypeExecAck    = "exec_ack"
	TypeMemReport    = "mem_report"
	TypeMemReportAck = "mem_report_ack"
	TypeAck   = "ack"
	TypeError = "error"
)

// Default per-message deadlines (docs/sandbox-runtime.md §4.9). Callers
// pass these to SetDeadline on the underlying conn covering dial+write+
// read of the management exchange. For launch/restore/attach the deadline
// covers only the handshake — the connection then lives on as the MUX.
const (
	DeadlineAppNotify = 200 * time.Millisecond // app_started / app_exited / launch_ack / mem_report
	DeadlinePing      = 200 * time.Millisecond
	DeadlineQuiesce   = 8 * time.Second // prep + stop-reading-app-pipes + MUX_CLOSE round-trip
	DeadlineRestore   = 5 * time.Second
	DeadlineAttach    = 5 * time.Second
	// DeadlineExec covers the exec handshake only (dial + exec →
	// exec_ack). Larger than restore/attach: the guest forks+execs the
	// child and resolves argv via PATH before acking. The conn's
	// deadline is cleared once it becomes the stdio MUX.
	DeadlineExec = 10 * time.Second
)

// --- Management wire format: [4 bytes LE length][JSON payload] --------

// WriteMessage writes one message in length-prefix-JSON wire format.
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
