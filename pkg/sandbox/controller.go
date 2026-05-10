package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/nodectl"
)

// ControllerHookOptions configures the dynamic-mode controller integration.
//
// SocketPath: UDS path of the controller endpoint. Empty disables
// the integration (sandbox runs in no-cgroup mode or static-cgroup mode).
//
// CHSocket: ch.sock path to send /api/v1/vm.resize when balloon target
// changes after Settled or Reclaim.
//
// CgroupPath: directory used for memory.high adjustments and
// memory.current reads.
type ControllerHookOptions struct {
	SocketPath string
	CHSocket   string
	CgroupPath string
	Logf       func(string, ...any)
}

// ControllerHooks bundles the per-sandbox state for dynamic-mode
// resource control: a connected client, current allocatable, and the
// background goroutines for Heartbeat and pressure-driven RequestBudget.
//
// The zero value with SocketPath empty is a safe no-op — every method
// becomes a noop. This lets lifecycle.go always call the hooks without
// branching on mode.
type ControllerHooks struct {
	opts ControllerHookOptions
	cfg  *SandboxConfig

	mu                sync.Mutex
	client            *nodectl.Client
	allocatableNowMem uint64
	released          bool
	cancelBg          context.CancelFunc
	bgWG              sync.WaitGroup
}

// NewControllerHooks dials the controller and returns hooks ready for
// Admit. Returns nil hooks (no-op) when SocketPath is empty.
func NewControllerHooks(opts ControllerHookOptions, cfg *SandboxConfig) (*ControllerHooks, error) {
	h := &ControllerHooks{opts: opts, cfg: cfg}
	if opts.SocketPath == "" {
		return h, nil
	}
	if h.opts.Logf == nil {
		h.opts.Logf = func(string, ...any) {}
	}
	h.client = &nodectl.Client{SocketPath: opts.SocketPath}
	if err := h.client.Connect(); err != nil {
		return nil, err
	}
	return h, nil
}

// Enabled reports whether dynamic mode is in effect.
func (h *ControllerHooks) Enabled() bool {
	return h != nil && h.client != nil
}

// AllocatableNowMem returns the current granted allocatable memory.
// In static or no-cgroup mode, returns 0.
func (h *ControllerHooks) AllocatableNowMem() uint64 {
	if !h.Enabled() {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allocatableNowMem
}

// Admit performs the controller handshake. Caller passes capacity /
// floor / startup_burst values from sandbox.yaml; allocatableAtSnapshot
// is non-zero only on the restore path.
//
// On success, the returned grantedInitialAlloc must be used to size
// initial cgroup memory.high and balloon target — it may be smaller
// than startup_burst when the controller had to degrade.
func (h *ControllerHooks) Admit(sid string, allocatableAtSnapshot uint64) (uint64, error) {
	if !h.Enabled() {
		// Mode A/B: no admission, return startup_burst (or floor if
		// startup_burst not configured) so the cold-start cgroup setup
		// works the same way.
		burst, err := h.cfg.StartupBurstBytes()
		if err != nil {
			return 0, err
		}
		return burst, nil
	}
	cap, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return 0, err
	}
	floor, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return 0, err
	}
	burst, err := h.cfg.StartupBurstBytes()
	if err != nil {
		return 0, err
	}
	floorCPU := h.cfg.Resources.Allocatable.CPU
	res, err := h.client.Admit(nodectl.AdmitParams{
		SandboxID:             sid,
		CapacityMemoryBytes:   cap,
		CapacityCPU:           h.cfg.Resources.Capacity.CPU,
		FloorMemoryBytes:      floor,
		FloorCPU:              floorCPU,
		StartupBudgetMemory:   burst,
		AllocatableAtSnapshot: allocatableAtSnapshot,
		CgroupPath:            h.cfg.Resources.Control.CgroupPath,
	})
	if err != nil {
		return 0, fmt.Errorf("admit: %w", err)
	}
	if res.Status == nodectl.StatusRejected {
		return 0, fmt.Errorf("admit rejected: %s", res.Msg)
	}
	if res.Status == nodectl.StatusQueued {
		return 0, fmt.Errorf("admit queued (eta %d ms); retry policy not yet implemented", res.QueuedETAMs)
	}
	h.mu.Lock()
	h.allocatableNowMem = res.GrantedInitialAlloc
	h.mu.Unlock()
	h.opts.Logf("controller admit: token=%s initial_alloc=%d", res.Token[:8], res.GrantedInitialAlloc)
	return res.GrantedInitialAlloc, nil
}

// Settled marks the cold-start launch hello arrival. Writes memory.high
// (in both static and dynamic modes) so cold boot's transient page-fault
// burst is not PSI-throttled — JoinCgroup deliberately defers the
// memory.high write to here. In dynamic mode, also notifies the
// controller and shrinks the CH balloon to the floor.
//
// Cold-start invariant: at this point, allocatable_now drops to the
// configured allocatable.memory floor.
func (h *ControllerHooks) Settled() error {
	if h == nil {
		return nil
	}
	// In no-cgroup mode (no CgroupPath), nothing to do here even if the
	// hooks struct is non-nil. setMemoryHigh below handles the path-empty
	// case as a no-op, but we still update allocatable_now state so
	// callers that read it (e.g. SettledRestore fallback) see the floor.
	floor, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return err
	}
	// (1) memory.high write — both modes. Independent of Enabled() because
	//     static-mode sandboxes still want PSI throttling once the boot
	//     transient is over; the only thing dynamic-mode adds is
	//     controller-driven dynamic reclaim, not the watermark itself.
	if err := h.setMemoryHigh(floor); err != nil {
		h.opts.Logf("settled: setMemoryHigh: %v", err)
	}
	if !h.Enabled() {
		// Static mode: no controller RPC, no dynamic balloon shrink. Done.
		h.mu.Lock()
		h.allocatableNowMem = floor
		h.mu.Unlock()
		return nil
	}
	// (2) dynamic-only: notify controller and shrink balloon to floor.
	rss := readMemoryCurrent(h.opts.CgroupPath)
	if err := h.client.Settled(rss, 0); err != nil {
		return fmt.Errorf("controller.Settled: %w", err)
	}
	if err := h.resizeBalloonTo(floor); err != nil {
		h.opts.Logf("settled: resizeBalloonTo: %v", err)
	}
	h.mu.Lock()
	h.allocatableNowMem = floor
	h.mu.Unlock()
	return nil
}

// SettledRestore is the restore-path settled trigger. Writes memory.high
// using the current allocatable_now (set by ApplyInitialAllocatable
// pre-resume). Unlike cold-start Settled, does NOT shrink balloon —
// the allocatable_at_snapshot value granted at Admit is preserved as
// runtime allocatable_now (balloon was already set by
// ApplyInitialAllocatable). In dynamic mode, also notifies controller.
func (h *ControllerHooks) SettledRestore() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	cur := h.allocatableNowMem
	h.mu.Unlock()
	if cur == 0 {
		// Fallback: no ApplyInitialAllocatable was called; use yaml floor.
		if floor, err := h.cfg.AllocatableMemoryBytes(); err == nil {
			cur = floor
		}
	}
	if err := h.setMemoryHigh(cur); err != nil {
		h.opts.Logf("settled-restore: setMemoryHigh: %v", err)
	}
	if !h.Enabled() {
		return nil
	}
	rss := readMemoryCurrent(h.opts.CgroupPath)
	if err := h.client.Settled(rss, 0); err != nil {
		return fmt.Errorf("controller.SettledRestore: %w", err)
	}
	return nil
}

// ApplyInitialAllocatable sets the runtime allocatable_now and the CH
// balloon target. Does NOT write memory.high — that is deferred to
// SettledRestore so restore-replay's uffd-driven page faults are not
// PSI-throttled (Issue 4 root cause).
//
// Used by restore (initialAlloc derived from snapshot) and dynamic-mode
// heartbeat-driven adjustments. In static mode caller passes
// max(yaml.allocatable, memory_resident) to avoid OOM during replay.
func (h *ControllerHooks) ApplyInitialAllocatable(initialAlloc uint64) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.allocatableNowMem = initialAlloc
	h.mu.Unlock()
	return h.resizeBalloonTo(initialAlloc)
}

// setMemoryHigh writes memory.high = allocBytes * watermark_ratio. The
// ratio is taken from the user-configured WatermarkHigh.memory if set,
// otherwise defaults to 0.875 (matching WatermarkHighBytes default).
// No-op when CgroupPath unset.
func (h *ControllerHooks) setMemoryHigh(allocBytes uint64) error {
	if h == nil || h.opts.CgroupPath == "" {
		return nil
	}
	ratio := 0.875
	if h.cfg.Resources.WatermarkHigh != nil && h.cfg.Resources.WatermarkHigh.Memory != "" {
		alloc, _ := h.cfg.AllocatableMemoryBytes()
		cur, perr := h.cfg.WatermarkHighBytes()
		if perr == nil && alloc > 0 {
			ratio = float64(cur) / float64(alloc)
		}
	}
	newHigh := uint64(float64(allocBytes) * ratio)
	path := filepath.Join(h.opts.CgroupPath, "memory.high")
	if err := os.WriteFile(path, []byte(strconv.FormatUint(newHigh, 10)), 0o644); err != nil {
		return fmt.Errorf("write memory.high: %w", err)
	}
	return nil
}

// resizeBalloonTo writes desired_balloon = capacity - allocBytes via CH
// /vm.resize. No-op when CHSocket unset.
func (h *ControllerHooks) resizeBalloonTo(allocBytes uint64) error {
	if h == nil || h.opts.CHSocket == "" {
		return nil
	}
	cap, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return err
	}
	target := cap - allocBytes
	if cap < allocBytes {
		target = 0
	}
	timing, err := chResizeBalloon(h.opts.CHSocket, target)
	if err != nil {
		return fmt.Errorf("vm.resize balloon: %w", err)
	}
	h.opts.Logf("balloon resize target=%dB ok (%s)", target, timing)
	return nil
}

// applyAllocatable writes both memory.high and balloon target. Used by
// heartbeat-driven adjustments (post-Settled), where both must move
// together to keep host PSI threshold consistent with guest free-page
// reporting target.
func (h *ControllerHooks) applyAllocatable(newAlloc uint64) error {
	if err := h.setMemoryHigh(newAlloc); err != nil {
		return err
	}
	return h.resizeBalloonTo(newAlloc)
}

// StartHeartbeat spawns the periodic Heartbeat goroutine. Stops on ctx
// cancel or Release.
//
// Each heartbeat returns the controller's authoritative allocatable_now.
// If it differs from the local value, the active reclaimer (or an
// admin reclaim command) shrank the sandbox; we apply the new value
// here (cgroup memory.high + balloon resize). Conversely, an admin
// grant grew it; same code path applies.
func (h *ControllerHooks) StartHeartbeat(ctx context.Context, period time.Duration) {
	if !h.Enabled() {
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if h.cancelBg == nil {
		h.cancelBg = cancel
	} else {
		old := h.cancelBg
		h.cancelBg = func() { old(); cancel() }
	}
	h.mu.Unlock()
	h.bgWG.Add(1)
	go func() {
		defer h.bgWG.Done()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				rss := readMemoryCurrent(h.opts.CgroupPath)
				res, err := h.client.Heartbeat(rss, 0, 0, 0)
				if err != nil {
					h.opts.Logf("heartbeat: %v (will retry next tick)", err)
					continue
				}
				if res != nil && res.NewAllocatable > 0 {
					h.mu.Lock()
					local := h.allocatableNowMem
					h.mu.Unlock()
					if res.NewAllocatable != local {
						h.opts.Logf("heartbeat: controller adjusted alloc %d → %d, applying",
							local, res.NewAllocatable)
						if err := h.ApplyInitialAllocatable(res.NewAllocatable); err != nil {
							h.opts.Logf("apply allocatable: %v", err)
						}
					}
				}
			}
		}
	}()
}

// Release sends a final Release message and tears down the connection.
// Idempotent.
func (h *ControllerHooks) Release(reason string) {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return
	}
	h.released = true
	h.mu.Unlock()
	if h.cancelBg != nil {
		h.cancelBg()
	}
	h.bgWG.Wait()
	if err := h.client.Release(reason); err != nil {
		h.opts.Logf("controller.Release: %v", err)
	}
	_ = h.client.Close()
}

// readMemoryCurrent reads memory.current from a cgroup directory. Returns
// 0 when the file is absent or the cgroup_path is empty.
func readMemoryCurrent(cgroupPath string) uint64 {
	if cgroupPath == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return 0
	}
	var n uint64
	for _, b := range data {
		if b < '0' || b > '9' {
			break
		}
		n = n*10 + uint64(b-'0')
	}
	return n
}

// chResizeBalloon issues PUT /api/v1/vm.resize with desired_balloon=
// to the CH api socket. Returns segmented timing so applyAllocatable
// can log it; previous "10s before fail" reports were not actionable
// because the timing was a single number with no breakdown.
func chResizeBalloon(sock string, desiredBalloonBytes uint64) (chAPITiming, error) {
	body := fmt.Sprintf(`{"desired_balloon":%d}`, desiredBalloonBytes)
	return chAPISendWithTiming(sock, "PUT", "/api/v1/vm.resize", body)
}

// chAPITiming records segmented latency for one chAPISend call. All
// fields are durations from the start of the call. Zero means "did not
// reach this stage". Surfaced via chAPISendWithTiming for diagnostics
// when investigating slow CH responses (e.g. the "10s before fail"
// reports — without this, we only see the 10s total, not where it
// went).
type chAPITiming struct {
	Dial      time.Duration
	Write     time.Duration
	FirstByte time.Duration
	Total     time.Duration
}

func (t chAPITiming) String() string {
	return fmt.Sprintf("dial=%s write=%s first_byte=%s total=%s",
		t.Dial.Truncate(time.Microsecond),
		t.Write.Truncate(time.Microsecond),
		t.FirstByte.Truncate(time.Microsecond),
		t.Total.Truncate(time.Microsecond))
}

func chAPISend(sock, method, path, body string) error {
	_, err := chAPISendWithTiming(sock, method, path, body)
	return err
}

// chAPISendWithTiming performs the same HTTP/1.1-over-UDS call as
// chAPISend but returns per-stage timings and surfaces read errors
// instead of silently truncating to whatever bytes arrived first. The
// previous implementation did `n, _ := c.Read(buf)` which discarded
// EOF/timeout/short-read errors, making it impossible to tell whether a
// "ch api short response" was caused by CH closing early, the deadline
// firing, or a single-Read returning partial bytes (HTTP/1.1 over a UDS
// can deliver headers and body across multiple read syscalls).
func chAPISendWithTiming(sock, method, path, body string) (chAPITiming, error) {
	t0 := time.Now()
	var t chAPITiming

	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Total = time.Since(t0)
		return t, fmt.Errorf("ch api dial %s: %w", sock, err)
	}
	t.Dial = time.Since(t0)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := c.Write([]byte(req)); err != nil {
		t.Total = time.Since(t0)
		return t, fmt.Errorf("ch api write: %w (timing %s)", err, t)
	}
	t.Write = time.Since(t0)

	// Read the response. CH replies with "HTTP/1.1 <code> ...\r\n\r\n"
	// (often empty body for 204). We stop as soon as we see the
	// header terminator "\r\n\r\n" or a Content-Length-bounded body
	// arrives — *not* waiting for EOF. CH keeps the conn open briefly
	// after a Connection: close response (~tens of ms typical, but
	// up to the read deadline observed empirically), so reading-to-EOF
	// would inflate every successful call to the deadline duration.
	//
	// Cap at maxResp bytes; CH responses are tiny.
	const maxResp = 16 * 1024
	buf := make([]byte, 0, maxResp)
	chunk := make([]byte, 4096)
	firstByteRecorded := false
	headersDone := false
	contentLen := -1
	bodyStart := -1
	var readErr error
	for {
		n, err := c.Read(chunk)
		if n > 0 && !firstByteRecorded {
			t.FirstByte = time.Since(t0)
			firstByteRecorded = true
		}
		if n > 0 {
			if len(buf)+n > maxResp {
				n = maxResp - len(buf)
			}
			buf = append(buf, chunk[:n]...)

			// Detect end of headers.
			if !headersDone {
				if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
					headersDone = true
					bodyStart = idx + 4
					contentLen = parseContentLength(buf[:idx])
				}
			}
			// If we have headers AND (no body expected, or full body
			// received), stop. CH 204 has Content-Length: 0 or absent;
			// CH 200 with payload has explicit Content-Length.
			if headersDone {
				bodyHave := len(buf) - bodyStart
				if contentLen <= 0 || bodyHave >= contentLen {
					break
				}
			}
			if len(buf) >= maxResp {
				break
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	t.Total = time.Since(t0)

	resp := string(buf)
	if len(resp) < 12 {
		// Surface the read error (was previously discarded).
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return t, fmt.Errorf("ch api %s %s read failed: %w (got %dB; timing %s)",
				method, path, readErr, len(buf), t)
		}
		return t, fmt.Errorf("ch api %s %s short response: %q (timing %s)",
			method, path, resp, t)
	}
	status := resp[9:12]
	if status[0] != '2' {
		return t, fmt.Errorf("ch api %s %s non-2xx: %q (timing %s)",
			method, path, resp, t)
	}
	return t, nil
}

// parseContentLength scans HTTP response headers for "Content-Length:".
// Returns -1 if not found. Header bytes are case-insensitive per RFC.
func parseContentLength(headers []byte) int {
	const key = "content-length:"
	lower := make([]byte, len(headers))
	for i, b := range headers {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		lower[i] = b
	}
	idx := bytes.Index(lower, []byte(key))
	if idx < 0 {
		return -1
	}
	// Skip key, then whitespace, then read digits to end of line.
	pos := idx + len(key)
	for pos < len(headers) && (headers[pos] == ' ' || headers[pos] == '\t') {
		pos++
	}
	val := 0
	for pos < len(headers) && headers[pos] >= '0' && headers[pos] <= '9' {
		val = val*10 + int(headers[pos]-'0')
		pos++
	}
	return val
}

// Suppress "unused" complaint when log import is not yet referenced
// elsewhere in this file (pulled in for diagnostics).
var _ = log.Default
