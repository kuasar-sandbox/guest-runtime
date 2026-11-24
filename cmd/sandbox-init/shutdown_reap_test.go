package main

import (
	"io"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestShutdownReapsPrimaryAfterEscalation(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; printf ready; exec sleep 30")
	ready, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err := io.ReadFull(ready, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	status, observed := waitOrTimeout(cmd.Process.Pid, 10*time.Millisecond, time.Second)
	if !observed || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("lost primary exit after escalation: observed=%v status=%v", observed, status)
	}
	var again syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &again, syscall.WNOHANG, nil); err != syscall.ECHILD {
		t.Fatalf("primary was not reaped: %v", err)
	}
}
