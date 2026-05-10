package sandbox

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// PingerConfig tunes the host→guest ping ticker. Defaults match
// docs/sandbox-runtime.md §4.5.
type PingerConfig struct {
	Interval time.Duration // default 1 s
	Timeout  time.Duration // default proto.DeadlinePing (200 ms)
}

func (c *PingerConfig) withDefaults() PingerConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = 1 * time.Second
	}
	if out.Timeout <= 0 {
		out.Timeout = proto.DeadlinePing
	}
	return out
}

// Pinger drives the periodic ping/pong probe against sandbox-init's
// reverse-channel listener. It owns a HostClient and a PingStats
// counter aggregator.
//
// Lifecycle (§9.1.4):
//
//   - Created by sandbox.Run / restore.Run with the per-sandbox vsock
//     base path.
//   - Start fires when the cold-start `launch` was written (or when a
//     `restored` was acked after restore).
//   - Pause / Resume bracket the snapshot quiesce window so we don't
//     race with /vm.pause.
//   - Stop is final — typically driven by ctx cancellation when CH
//     exits.
type Pinger struct {
	Client *HostClient
	Cfg    PingerConfig
	Stats  *PingStats
	Logf   func(string, ...any)

	mu       sync.Mutex
	running  atomic.Bool
	paused   atomic.Bool
	cancel   context.CancelFunc
	doneCh   chan struct{}
	nextID   atomic.Uint64
}

// Start launches the ticker goroutine. ctx cancellation stops the
// ticker. Calling Start more than once is a no-op until Stop is called.
func (p *Pinger) Start(ctx context.Context) {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	if p.Logf == nil {
		p.Logf = func(string, ...any) {}
	}
	cfg := p.Cfg.withDefaults()
	ctx, cancel := context.WithCancel(ctx)

	p.mu.Lock()
	p.cancel = cancel
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go p.loop(ctx, cfg)
}

// Stop signals the ticker to exit and waits for it. Idempotent.
func (p *Pinger) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}
	p.mu.Lock()
	cancel := p.cancel
	doneCh := p.doneCh
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if doneCh != nil {
		<-doneCh
	}
}

// Pause halts ping send temporarily without tearing the goroutine down.
// Used during snapshot quiesce window so the host doesn't dial guest
// while it's pre-paused (§9.1.4 quiesce → /vm.pause sequence).
func (p *Pinger) Pause()  { p.paused.Store(true) }
func (p *Pinger) Resume() { p.paused.Store(false) }

// Running reports whether the ticker is active (Start called, Stop not yet).
func (p *Pinger) Running() bool { return p.running.Load() }

func (p *Pinger) loop(ctx context.Context, cfg PingerConfig) {
	defer func() {
		p.mu.Lock()
		dc := p.doneCh
		p.mu.Unlock()
		if dc != nil {
			close(dc)
		}
	}()

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()

	// Fire first ping immediately — we want sub-ms detection of "guest
	// agent ready" right after launch is sent.
	p.tick(cfg.Timeout)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.paused.Load() {
				continue
			}
			p.tick(cfg.Timeout)
		}
	}
}

func (p *Pinger) tick(timeout time.Duration) {
	if p.Stats == nil {
		p.Stats = &PingStats{}
	}
	id := p.nextID.Add(1)
	tSend := time.Now()
	p.Stats.Attempts.Add(1)
	resp, err := p.Client.RoundTrip(&proto.Message{
		Type:    proto.TypePing,
		ID:      id,
		TSendNs: tSend.UnixNano(),
	}, timeout)
	if err != nil {
		// Best-effort classification — RoundTrip returns wrapped errors.
		// We treat any error containing "deadline" / "i/o timeout" as a
		// ping_timeout and everything else as a dial error. This keeps
		// the metric meaningful without requiring a typed-error sprawl.
		s := err.Error()
		if containsAny(s, "deadline", "i/o timeout", "timed out") {
			p.Stats.Timeout.Add(1)
		} else {
			p.Stats.DialError.Add(1)
		}
		p.Logf("ping id=%d err=%v", id, err)
		return
	}
	if resp.Type != proto.TypePong || resp.ID != id {
		p.Stats.DialError.Add(1)
		p.Logf("ping id=%d unexpected resp %+v", id, resp)
		return
	}
	p.Stats.Success.Add(1)
	p.Stats.observeRTT(time.Since(tSend))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is a 0-dep substring search to avoid pulling in strings.Contains
// in this file (kept package-local; pkg/sandbox already imports strings
// elsewhere — this is just to keep the helper close to its caller).
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// SendQuiesce performs a host→guest quiesce request/response on a
// short-lived connection. Caller is expected to have paused the ping
// ticker first to avoid concurrent host→guest traffic during the
// snapshot pre-pause window.
func SendQuiesce(client *HostClient) error {
	resp, err := client.RoundTrip(&proto.Message{Type: proto.TypeQuiesce}, proto.DeadlineQuiesce)
	if err != nil {
		return err
	}
	if resp.Type != proto.TypeQuiesced {
		return &protoMismatchErr{want: proto.TypeQuiesced, got: resp.Type, msg: resp.Msg}
	}
	return nil
}

// SendRestore notifies the guest that a restore has completed. epoch
// distinguishes successive restores. Caller (re)starts the ping ticker
// after this returns nil.
func SendRestore(client *HostClient, epoch uint32) error {
	resp, err := client.RoundTrip(&proto.Message{Type: proto.TypeRestore, Epoch: epoch}, proto.DeadlineRestore)
	if err != nil {
		return err
	}
	if resp.Type != proto.TypeRestored {
		return &protoMismatchErr{want: proto.TypeRestored, got: resp.Type, msg: resp.Msg}
	}
	return nil
}

type protoMismatchErr struct {
	want, got, msg string
}

func (e *protoMismatchErr) Error() string {
	if e.msg != "" {
		return "proto: want " + e.want + ", got " + e.got + " (msg=" + e.msg + ")"
	}
	return "proto: want " + e.want + ", got " + e.got
}
