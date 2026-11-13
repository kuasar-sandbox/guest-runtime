package main

import (
	"errors"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

func TestInitialLaunchOwnerIsMonotonic(t *testing.T) {
	var o initialLaunchOwner
	o.begin("launch-one")
	if s := o.snapshot(); s.State != proto.InitialLaunchPending || s.Token != "launch-one" {
		t.Fatal(s)
	}
	o.started(42)
	// A normal restart must not replace the initial PID/fact.
	o.started(43)
	if s := o.snapshot(); s.PID != 42 || s.State != proto.InitialLaunchStarted {
		t.Fatal(s)
	}
	o.exited(7)
	o.started(44)
	if s := o.snapshot(); s.State != proto.InitialLaunchExited || s.Code != 7 {
		t.Fatal(s)
	}
	o = initialLaunchOwner{}
	o.begin("new-prewarmed-launch")
	o.failed(errors.New("exec sync failed"))
	o.started(45)
	o.exited(0)
	if s := o.snapshot(); s.Token != "new-prewarmed-launch" || s.State != proto.InitialLaunchFailed || s.Error != "exec sync failed" {
		t.Fatal(s)
	}
	o = initialLaunchOwner{}
	o.begin("")
	if o.snapshot() != nil {
		t.Fatal("old Host was forced into token protocol")
	}
}

func TestInitialLaunchSnapshotConsistentDuringTermination(t *testing.T) {
	var o initialLaunchOwner
	o.begin("current")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				s := o.snapshot()
				if s.Token != "current" || (s.State == proto.InitialLaunchStarted && s.PID != 42) {
					t.Error(s)
				}
			}
		})
	}
	o.started(42)
	o.exited(9)
	wg.Wait()
	if s := o.snapshot(); s.State != proto.InitialLaunchExited || s.Code != 9 {
		t.Fatal(s)
	}
}

func TestPongCarriesInitialFactWithoutSupervisor(t *testing.T) {
	initialLaunch = initialLaunchOwner{}
	initialLaunch.begin("prewarm-token")
	defer func() { initialLaunch = initialLaunchOwner{} }()
	for _, state := range []string{proto.InitialLaunchPending, proto.InitialLaunchStarted, proto.InitialLaunchExited} {
		if state == proto.InitialLaunchStarted {
			initialLaunch.started(42)
		}
		if state == proto.InitialLaunchExited {
			initialLaunch.exited(5)
		}
		guest, host := usageSocketPair(t)
		done := make(chan struct{})
		go func() { defer close(done); defer guest.Close(); handleReverseConnShutdown(guest, nil, nil, func() {}) }()
		if err := proto.WriteMessage(host, &proto.Message{Type: proto.TypePing, ID: 31}); err != nil {
			t.Fatal(err)
		}
		resp, err := proto.ReadMessage(host)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Type != proto.TypePong || resp.ID != 31 || resp.InitialLaunchState == nil || resp.InitialLaunchState.Token != "prewarm-token" || resp.InitialLaunchState.State != state {
			t.Fatal(resp)
		}
		host.Close()
		<-done
	}
}

func TestShutdownBeforeInitialTokenBindingRemainsTerminal(t *testing.T) {
	var o initialLaunchOwner
	o.exited(9)
	if o.snapshot() != nil {
		t.Fatal("unbound terminal fact exposed")
	}
	o.begin("current")
	o.started(42)
	if s := o.snapshot(); s.Token != "current" || s.State != proto.InitialLaunchExited || s.Code != 9 {
		t.Fatalf("shutdown was erased by launch: %+v", s)
	}
	o.begin("replacement")
	if s := o.snapshot(); s.Token != "current" || s.State != proto.InitialLaunchExited {
		t.Fatalf("second token replaced initial launch: %+v", s)
	}
}
