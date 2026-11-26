package main

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

func restartSystemdRestoreTarget(ctx context.Context) error {
	fd, err := connectSystemdManager(ctx)
	if err != nil {
		return fmt.Errorf("restore systemd connection: %w", err)
	}
	// Inner shares the manager's PID namespace; the expected peer is PID 1.
	peer, err := openSystemdPeer(ctx, fd, 1)
	if err != nil {
		return fmt.Errorf("restore systemd peer: %w", err)
	}
	defer peer.close()
	stop := context.AfterFunc(ctx, peer.close)
	defer stop()
	return peer.restartRestoreTarget(ctx)
}

func (p *systemdPeer) restartRestoreTarget(ctx context.Context) error {
	p.deadline(ctx)
	// Register before Subscribe/RestartUnit: even an empty target may finish
	// before the method reply. This connection is private to this invocation.
	signals := make(chan *dbus.Signal, 32)
	p.bus.Signal(signals)
	defer p.bus.RemoveSignal(signals)
	manager := p.bus.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	if err := manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.Subscribe", 0).Err; err != nil {
		return fmt.Errorf("subscribe restore jobs: %w", err)
	}
	var job dbus.ObjectPath
	if err := manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.RestartUnit", 0, systemdRestoreTarget, "replace").Store(&job); err != nil {
		return fmt.Errorf("restart %s: %w", systemdRestoreTarget, err)
	}
	return waitSystemdRestoreJob(ctx, signals, job)
}

func waitSystemdRestoreJob(ctx context.Context, signals <-chan *dbus.Signal, job dbus.ObjectPath) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait %s: %w", systemdRestoreTarget, ctx.Err())
		case sig, ok := <-signals:
			if !ok {
				return fmt.Errorf("%s: systemd connection closed", systemdRestoreTarget)
			}
			if sig == nil || sig.Path != "/org/freedesktop/systemd1" || sig.Name != "org.freedesktop.systemd1.Manager.JobRemoved" || len(sig.Body) != 4 {
				continue
			}
			path, pathOK := sig.Body[1].(dbus.ObjectPath)
			unit, unitOK := sig.Body[2].(string)
			result, resultOK := sig.Body[3].(string)
			if !pathOK || path != job || !unitOK || unit != systemdRestoreTarget {
				continue
			}
			if !resultOK || result != "done" {
				return fmt.Errorf("%s job %s: %v", systemdRestoreTarget, job, sig.Body[3])
			}
			// Do not publish success when cancellation raced the final signal.
			return ctx.Err()
		}
	}
}
