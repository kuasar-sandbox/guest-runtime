package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// runWithTimeout is the sole child waiter during the serial init phase. It
// observes exits without consuming them, then either lets Cmd.Wait collect the
// current command or reaps an adopted orphan. Keeping both actions in this loop
// preserves exec.ExitError and ProcessState without competing wait goroutines.
// The caller supplies file-backed stdio and starts no other supervised jobs.
func runWithTimeout(cmd *exec.Cmd, timeoutMs int64) error {
	children := make(chan os.Signal, 1)
	signal.Notify(children, syscall.SIGCHLD)
	defer signal.Stop(children)
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	var deadline <-chan time.Time
	if timeoutMs > 0 {
		timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer timer.Stop()
		deadline = timer.C
	}
	timedOut := false
	expire := func() {
		// Preserve the existing timeout scope: kill this init command,
		// not its process group or descendants, and collect its exit.
		timedOut = true
		deadline = nil
		_ = cmd.Process.Kill()
	}
	for {
		// A stream of orphan exits must not postpone the command deadline.
		select {
		case <-deadline:
			expire()
		default:
		}
		pid, err := nextExitedInitChild()
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return os.NewSyscallError("waitid init", err)
		}
		if pid == cmd.Process.Pid {
			waited = true
			err := cmd.Wait()
			if timedOut {
				return fmt.Errorf("timed out after %dms", timeoutMs)
			}
			// A later init command or the normal supervisor takes over any
			// remaining orphans. No waiter outlives this serial phase.
			return err
		}
		if pid != 0 {
			if err := reapInitOrphan(pid); err != nil {
				return err
			}
			continue
		}
		select {
		case <-children:
		case <-deadline:
			expire()
		}
	}
}

// nextExitedInitChild leaves the exit waitable so Cmd.Wait can retain the
// current init command's native ProcessState. x/sys exposes the common siginfo
// header but not the SIGCHLD union. Linux aligns that union after three int32s
// to a machine word (offset 16 on the supported amd64/arm64 targets).
func nextExitedInitChild() (int, error) {
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
		return 0, err
	}
	if info.Signo == 0 {
		return 0, nil
	}
	type childInfo struct {
		_   [3]int32
		_   [unsafe.Sizeof(uintptr(0)) / 8]int32
		pid int32
	}
	return int((*childInfo)(unsafe.Pointer(&info)).pid), nil
}

func reapInitOrphan(pid int) error {
	for {
		_, err := syscall.Wait4(pid, nil, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return os.NewSyscallError("wait4 init orphan", err)
		}
		return nil
	}
}
