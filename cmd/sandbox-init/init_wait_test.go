package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	initWaitTestMode = "SANDBOX_INIT_TEST_WAIT_MODE"
	initWaitTestRole = "SANDBOX_INIT_TEST_WAIT_ROLE"
	initWaitTestDir  = "SANDBOX_INIT_TEST_WAIT_DIR"
)

// The init phase owns all of its process's children, so exercise it in another
// process rather than letting waitid(P_ALL) observe the Go test runner's jobs.
func TestInitWait(t *testing.T) {
	for _, mode := range []string{"success", "exit", "signal", "timeout", "orphan", "timeout-descendant"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInitWaitHelper$")
			cmd.Env = append(os.Environ(), initWaitTestMode+"="+mode, initWaitTestDir+"="+t.TempDir())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated %s: %v\n%s", mode, err, out)
			}
		})
	}
}

func TestInitWaitHelper(t *testing.T) {
	mode := os.Getenv(initWaitTestMode)
	if mode == "" {
		t.Skip("isolated init waiter helper")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	var timeoutMs int64
	switch mode {
	case "success":
		cmd = exec.Command("/bin/sh", "-c", "exit 0")
	case "exit":
		cmd = exec.Command("/bin/sh", "-c", "exit 37")
	case "signal":
		cmd = exec.Command("/bin/sh", "-c", "kill -TERM $$")
	case "timeout":
		cmd = initWaitChildCommand("hold")
		timeoutMs = 100
	case "orphan":
		cmd = initWaitChildCommand("orphan-init")
	case "timeout-descendant":
		cmd = initWaitChildCommand("hold-parent")
		timeoutMs = 1000
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := runWithTimeout(cmd, timeoutMs)
	if cmd.ProcessState == nil {
		t.Fatalf("missing native command ProcessState: %v", err)
	}
	switch mode {
	case "success", "orphan":
		if err != nil || !cmd.ProcessState.Success() {
			t.Fatalf("command failed: %v, %v", cmd.ProcessState, err)
		}
	case "exit":
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 37 || cmd.ProcessState.ExitCode() != 37 {
			t.Fatalf("exit status was not preserved: %v, %v", cmd.ProcessState, err)
		}
	case "signal":
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("signal was not preserved: %v, %v", cmd.ProcessState, err)
		}
	case "timeout", "timeout-descendant":
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("timed out after %dms", timeoutMs)) {
			t.Fatalf("timeout result: %v", err)
		}
		if got := cmd.ProcessState.Sys().(syscall.WaitStatus).Signal(); got != syscall.SIGKILL {
			t.Fatalf("timed out command signal = %v", got)
		}
	}
	if mode == "timeout-descendant" {
		pid := readInitWaitChildPID(t)
		// Timeout historically kills only the init command. The descendant
		// remains alive and is now the stage owner's adopted child.
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("timeout killed descendant: %v", err)
		}
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil || info.Signo != 0 {
			t.Fatalf("timeout left descendant exited: signo=%d, err=%v", info.Signo, err)
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if err := reapInitOrphan(pid); err != nil {
			t.Fatal(err)
		}
	}
	// No waiter survives runWithTimeout to steal a later supervisor's child.
	next := exec.Command("/bin/sh", "-c", "exit 43")
	var exit *exec.ExitError
	if err := next.Run(); !errors.As(err, &exit) || exit.ExitCode() != 43 {
		t.Fatalf("next phase lost its child: %v", err)
	}
	if _, err := nextExitedInitChild(); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("children left after init phase: %v", err)
	}
}

func initWaitChildCommand(role string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestInitWaitChild$")
	cmd.Env = append(os.Environ(), initWaitTestRole+"="+role)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd
}

func readInitWaitChildPID(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv(initWaitTestDir), "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("child pid %q: %v", data, err)
	}
	return pid
}

func TestInitWaitChild(t *testing.T) {
	switch role := os.Getenv(initWaitTestRole); role {
	case "":
		t.Skip("init command helper")
	case "hold":
		time.Sleep(30 * time.Second)
	case "orphan-child":
		time.Sleep(30 * time.Millisecond)
		os.Exit(29)
	case "orphan-parent", "hold-parent":
		childRole := "orphan-child"
		if role == "hold-parent" {
			childRole = "hold"
		}
		child := initWaitChildCommand(childRole)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv(initWaitTestDir), "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
		_ = child.Process.Release()
		if role == "orphan-parent" {
			os.Exit(11)
		}
		time.Sleep(30 * time.Second)
	case "orphan-init":
		var exit *exec.ExitError
		if err := initWaitChildCommand("orphan-parent").Run(); !errors.As(err, &exit) || exit.ExitCode() != 11 {
			t.Fatalf("intermediate parent: %v", err)
		}
		pid := readInitWaitChildPID(t)
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); errors.Is(err, os.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("orphan pid %d was not reaped while init was running", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	default:
		t.Fatalf("unknown child role %q", role)
	}
}
