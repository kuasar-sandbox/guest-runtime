package sandbox

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// TestPinger_TickAndPause runs a fake guest behind a fakeCHProxy and
// verifies the ticker fires, RTT samples accumulate, and Pause stops
// new attempts without tearing the goroutine down.
func TestPinger_TickAndPause(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var seen atomic.Uint64
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			return
		}
		if req.Type != proto.TypePing {
			t.Errorf("guest got %s", req.Type)
			return
		}
		seen.Add(1)
		_ = proto.WriteMessage(c, &proto.Message{
			Type:    proto.TypePong,
			ID:      req.ID,
			TSendNs: req.TSendNs,
		})
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg:    PingerConfig{Interval: 30 * time.Millisecond, Timeout: 200 * time.Millisecond},
		Stats:  &PingStats{},
		Logf:   t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p.Start(ctx)
	defer p.Stop()

	// Wait for at least 3 pings.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if seen.Load() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seen.Load() < 3 {
		t.Fatalf("only %d pings seen", seen.Load())
	}

	p.Pause()
	frozen := seen.Load()
	time.Sleep(150 * time.Millisecond)
	if seen.Load() > frozen+1 {
		// One more tick may slip in if Pause races with the goroutine
		// already inside RoundTrip; allow at most +1.
		t.Errorf("pause did not stop pings: %d -> %d", frozen, seen.Load())
	}

	p.Resume()
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if seen.Load() > frozen+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seen.Load() <= frozen+1 {
		t.Errorf("resume did not restart pings: %d -> %d", frozen, seen.Load())
	}

	snap := p.Stats.Snapshot()
	if snap.Success == 0 {
		t.Errorf("Snapshot.Success = 0")
	}
	if snap.RTTAvgNs == 0 {
		t.Errorf("Snapshot.RTTAvgNs = 0")
	}
}

func TestSendQuiesce_Quiesced(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, _ := proto.ReadMessage(c)
		if req.Type != proto.TypeQuiesce {
			t.Errorf("got %s", req.Type)
		}
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeQuiesced})
	})
	defer proxy.close()

	if err := SendQuiesce(&HostClient{BasePath: base}); err != nil {
		t.Fatal(err)
	}
}

func TestSendRestore_Restored(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, _ := proto.ReadMessage(c)
		if req.Type != proto.TypeRestore || req.Epoch != 3 {
			t.Errorf("got %+v", req)
		}
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeRestored, Epoch: req.Epoch})
	})
	defer proxy.close()

	if err := SendRestore(&HostClient{BasePath: base}, 3); err != nil {
		t.Fatal(err)
	}
}

func TestSendQuiesce_WrongResponse(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "bad"})
	})
	defer proxy.close()

	err := SendQuiesce(&HostClient{BasePath: base})
	if err == nil {
		t.Error("expected error on wrong response")
	}
}
