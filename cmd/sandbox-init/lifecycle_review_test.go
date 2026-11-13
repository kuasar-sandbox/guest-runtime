package main

import (
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux"
)

func TestQuiesceJoinsAdmittedRestart(t *testing.T) {
	var sup supervisorState
	if !sup.admitRestart() {
		t.Fatal("initial restart admission failed")
	}
	drained := make(chan struct{})
	go func() {
		sup.closeRestartAdmission()
		close(drained)
	}()
	deadline := time.Now().Add(time.Second)
	for !sup.quiescing.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !sup.quiescing.Load() {
		sup.restartMu.Unlock()
		t.Fatal("restart admission did not close")
	}
	select {
	case <-drained:
		sup.restartMu.Unlock()
		t.Fatal("quiesce completed before the admitted restart finished")
	default:
	}
	sup.restartMu.Unlock()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("quiesce did not finish after restart left its handshake")
	}
	sup.quiescing.Store(false)
	if !sup.admitRestart() {
		t.Fatal("restart admission did not reopen after thaw")
	}
	sup.restartMu.Unlock()
}

func TestExecReapDiscardsUnregisteredChildren(t *testing.T) {
	reg := newExecRegistry()
	unregistered := exec.Command("sh", "-c", "exit 7")
	if err := unregistered.Start(); err != nil {
		t.Fatal(err)
	}
	registered := exec.Command("sh", "-c", "exit 13")
	wait, err := reg.startChild(registered)
	if err != nil {
		_ = unregistered.Wait()
		t.Fatal(err)
	}
	defer unregistered.Process.Release()
	defer registered.Process.Release()
	seen := make(map[int]bool)
	deadline := time.Now().Add(time.Second)
	for len(seen) != 2 && time.Now().Before(deadline) {
		pid, status, wasExec, err := reg.reapChild()
		if err != nil {
			t.Fatal(err)
		}
		switch pid {
		case 0:
			time.Sleep(time.Millisecond)
		case unregistered.Process.Pid:
			if wasExec || status.ExitStatus() != 7 {
				t.Fatalf("unregistered child reaped as exec=%t, status=%v", wasExec, status)
			}
			seen[pid] = true
		case registered.Process.Pid:
			if !wasExec || status.ExitStatus() != 13 {
				t.Fatalf("exec child reaped as exec=%t, status=%v", wasExec, status)
			}
			seen[pid] = true
		default:
			t.Fatalf("unexpected child %d", pid)
		}
	}
	if len(seen) != 2 || len(reg.waiters) != 0 {
		t.Fatalf("reaped %d children; %d waiters remain", len(seen), len(reg.waiters))
	}
	select {
	case st := <-wait:
		if st.ExitStatus() != 13 {
			t.Fatalf("exec waiter got %d, want 13", st.ExitStatus())
		}
	default:
		t.Fatal("missing exec status")
	}
}

func TestFinishExecMuxDeliversStatusBeforeClose(t *testing.T) {
	g, h := net.Pipe()
	streams := mux.PipeStreams(false, false, false)
	guest := mux.NewSession(g, streams, mux.Options{})
	host := mux.NewSession(h, streams, mux.Options{})
	defer host.Close()
	if err := finishExecMux(guest, 17, time.Second); err != nil {
		t.Fatal(err)
	}
	if code, ok := host.ExitStatus(); !ok || code != 17 {
		t.Fatalf("host exit status = %d, %t; want 17, true", code, ok)
	}
}

func TestFinishExecMuxHonorsShutdownDeadline(t *testing.T) {
	g, h := net.Pipe()
	defer h.Close()
	guest := mux.NewSession(g, mux.PipeStreams(false, false, false), mux.Options{})
	done := make(chan error, 1)
	go func() { done <- finishExecMux(guest, 17, 20*time.Millisecond) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing peer acknowledgement reported as success")
		}
	case <-time.After(time.Second):
		t.Fatal("session remained open after shutdown deadline")
	}
}
