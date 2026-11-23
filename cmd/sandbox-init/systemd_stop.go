package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

type environmentStopProof uint8

const (
	stopUnverified environmentStopProof = iota
	stopNormal
	stopFailed
)

// The first cause owns the result. A signal or executor EOF is not, by itself,
// evidence that systemd was shutting down. In particular, a poweroff requested
// by this process must never turn the original failure into a successful exit.
type environmentStopFact struct {
	reason            string
	proof             environmentStopProof
	poweroffRequested bool
	forced            bool
}

func (c *executorClient) beginEnvironmentStop(reason string, proof environmentStopProof) {
	c.stopMu.Lock()
	if !c.stopping.CompareAndSwap(false, true) {
		c.stopMu.Unlock()
		return
	}
	c.stopFact = environmentStopFact{reason: reason, proof: proof}
	deadline := time.Now().Add(systemdGrace(c.spec) + 5*time.Second)
	c.stopMu.Unlock()
	// The latch and absolute timer precede every lock, query, thaw and write.
	// Only the actual PID 1 Wait retires this environment lifetime budget.
	go func() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
			c.stopMu.Lock()
			select {
			case <-c.processDone:
			default:
				// A successful escalation remains a failed graceful outcome,
				// even if a concurrent manager exit happens to look clean.
				if err := c.process.Kill(); err == nil {
					c.stopFact.forced = true
				}
			}
			c.stopMu.Unlock()
		case <-c.processDone:
		}
	}()
	go func() {
		logf("environment stopping: %s", reason)
		if c.systemd != nil && proof == stopUnverified {
			c.proveEnvironmentStop()
		}
		c.stopEnvironment()
	}()
}

func (c *executorClient) proveEnvironmentStop() {
	observer := c.systemd.observer.Load()
	if observer == nil {
		return
	}
	// Query the independent manager peer before requesting poweroff. Queueing
	// behind an existing observation consumes this same one-second budget.
	ctx, cancel := context.WithTimeout(context.Background(), systemdObserveQuery)
	defer cancel()
	state, err := observer.inspectStop(ctx)
	if err != nil {
		logf("systemd stop observation unavailable: %v", err)
		return
	}
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	if c.stopFact.proof != stopUnverified || c.stopFact.poweroffRequested {
		return
	}
	if state == "stopping" {
		c.stopFact.proof = stopNormal
	} else {
		c.stopFact.proof = stopFailed
	}
}

func (c *executorClient) stopEnvironment() {
	// The observer and stop latch never wait for this management boundary.
	// Quiesce can be joining an observer attempt while it holds the lock.
	managementLifecycle.Lock()
	defer managementLifecycle.Unlock()
	guestSlots.resumeMounts()
	if err := cgroupThaw(); err != nil {
		logf("shutdown thaw: %v", err)
	}
	if c.systemd != nil {
		c.stopMu.Lock()
		select {
		case <-c.processDone:
			c.stopMu.Unlock()
			return
		default:
		}
		c.stopFact.poweroffRequested = true
		c.stopMu.Unlock()
		// Drain the execution service and manager in parallel. systemd treats
		// SIGTERM as reexec; libc SIGRTMIN+4 is the poweroff request.
		_ = unix.PidfdSendSignal(c.systemd.executorPIDFD, unix.SIGTERM, nil, 0)
		if err := c.process.Signal(systemdPoweroffSignal); err != nil {
			logf("systemd poweroff signal: %v", err)
		}
		return
	}
	if c.ready.Load() {
		if err := c.process.Signal(syscall.SIGTERM); err != nil {
			_ = c.process.Kill()
		}
	} else {
		_ = c.process.Kill()
	}
}

func environmentWaitResult(err error) (code, signal int) {
	if err == nil {
		return 0, 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal = int(status.Signal())
		}
		return code, signal
	}
	return -1, 0
}

func classifyEnvironmentExit(fact environmentStopFact, code, signal int) proto.EnvironmentExit {
	result := proto.EnvironmentExit{State: proto.EnvironmentFailed, Code: code, TermSignal: signal, Reason: fact.reason}
	if result.Reason == "" {
		result.Reason = "systemd manager exited without a stop request"
	}
	normalWait := (code == 0 && signal == 0) || signal == int(syscall.SIGINT)
	if fact.forced || fact.proof == stopFailed || !normalWait {
		return result
	}
	if fact.proof == stopNormal {
		result.State = proto.EnvironmentStopped
	} else if fact.proof == stopUnverified && !fact.poweroffRequested {
		// Linux reports a PID-namespace POWER_OFF as SIGINT to the parent.
		// This real Wait can prove a fast voluntary shutdown that completed
		// between observer ticks, only if we did not cause that poweroff.
		result.State = proto.EnvironmentStopped
	}
	return result
}

func (c *executorClient) reportEnvironmentExit() {
	if c.systemd == nil {
		return
	}
	<-c.processDone
	code, signal := environmentWaitResult(c.processErr)
	c.stopMu.Lock()
	result := classifyEnvironmentExit(c.stopFact, code, signal)
	c.stopMu.Unlock()
	logf("environment exited state=%s code=%d signal=%d reason=%s", result.State, result.Code, result.TermSignal, result.Reason)
	if err := notifyEnvironmentExit(result); err != nil {
		logf("environment exit notification failed: %v", err)
	}
}
