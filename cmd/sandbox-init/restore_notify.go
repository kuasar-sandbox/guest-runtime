package main

import (
	"errors"
	"syscall"
)

// primaryMu pins all registered children against Wait4/PID reuse. Restart
// admission remains closed until prepareReattach finishes this notification.
func (s *supervisorState) notifyRestore(send func(int, syscall.Signal) error) {
	s.primaryMu.RLock()
	defer s.primaryMu.RUnlock()
	notify := func(pid, signal int) {
		if pid <= 0 || signal == 0 {
			return
		}
		if err := send(pid, syscall.Signal(signal)); err != nil && !errors.Is(err, syscall.ESRCH) {
			logf("restore signal %d to pid=%d: %v (continuing)", signal, pid, err)
		}
	}
	if s.spec != nil {
		notify(int(s.appPid.Load()), s.spec.RestoreSignal)
	}
	if s.pluginReg == nil {
		return
	}
	s.pluginReg.mu.Lock()
	defer s.pluginReg.mu.Unlock()
	for pid, p := range s.pluginReg.byPID {
		notify(pid, p.spec.RestoreSignal)
	}
}
