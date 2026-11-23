package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	systemdObservePeriod  = time.Second
	systemdObserveQuery   = time.Second
	systemdObserveFailure = 5 * time.Second
)

type observerOperation uint8

const (
	observerPause observerOperation = iota
	observerResume
	observerInspectStop
)

type observerResult struct {
	state string
	err   error
}
type observerCommand struct {
	op     observerOperation
	ctx    context.Context
	result chan observerResult
}

type observedSystemdPeer interface {
	state(context.Context) (string, error)
	close()
}

// One loop owns the private peer and its health clock. Freeze joins the current
// attempt before preserving the remaining clock; stop inspection shares this
// peer without requiring a live executor. The expiry timer is independent of
// poll ticks, so fast failed queries cannot extend last-valid-reply + five s.
type systemdObserver struct {
	client    *executorClient
	commands  chan observerCommand
	done      chan struct{}
	peer      observedSystemdPeer
	deadline  time.Time
	remaining time.Duration
	paused    bool
	stop      func(string, environmentStopProof) // observation-to-lifecycle boundary
}

func newSystemdObserver(c *executorClient) *systemdObserver {
	return &systemdObserver{client: c, commands: make(chan observerCommand), done: make(chan struct{}), peer: c.systemd.peer, deadline: time.Now().Add(systemdObserveFailure)}
}
func (o *systemdObserver) start() {
	if o.stop == nil {
		o.stop = o.client.beginEnvironmentStop
	}
	go o.run()
}

func (o *systemdObserver) command(ctx context.Context, op observerOperation) (string, error) {
	reply := make(chan observerResult, 1)
	select {
	case o.commands <- observerCommand{op: op, ctx: ctx, result: reply}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-o.done:
		return "", errors.New("systemd observer stopped")
	}
	select {
	case result := <-reply:
		return result.state, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-o.done:
		return "", errors.New("systemd observer stopped")
	}
}
func (o *systemdObserver) request(pause bool) error {
	op := observerResume
	if pause {
		op = observerPause
	}
	_, err := o.command(context.Background(), op)
	return err
}
func (o *systemdObserver) inspectStop(ctx context.Context) (string, error) {
	return o.command(ctx, observerInspectStop)
}

func validSystemdState(state string) bool {
	switch state {
	case "stopping", "initializing", "starting", "running", "degraded", "maintenance", "reloading":
		return true
	default:
		return false
	}
}

func (o *systemdObserver) closePeer() {
	if o.peer != nil {
		o.peer.close()
		o.peer = nil
	}
}
func (o *systemdObserver) queryState(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var err error
	if o.peer == nil {
		var peer *systemdPeer
		peer, err = o.client.refreshSystemdPeer(ctx)
		if err == nil {
			o.peer = peer
		}
	}
	var state string
	if err == nil {
		state, err = o.peer.state(ctx)
	}
	if err == nil && !validSystemdState(state) {
		err = fmt.Errorf("unknown systemd manager state %q", state)
	}
	if err != nil {
		o.closePeer()
		return "", err
	}
	return state, nil
}

func (o *systemdObserver) query() error {
	remaining := time.Until(o.deadline)
	if remaining <= 0 {
		return errors.New("systemd manager observation budget expired")
	}
	ctx, cancel := context.WithTimeout(context.Background(), min(systemdObserveQuery, remaining))
	defer cancel()
	state, err := o.queryState(ctx)
	if err != nil {
		return err
	}
	if !time.Now().Before(o.deadline) {
		return errors.New("systemd manager replied after the observation deadline")
	}
	if state == "stopping" {
		o.stop("manager stopping", stopNormal)
	}
	o.deadline = time.Now().Add(systemdObserveFailure)
	return nil
}

func (o *systemdObserver) resume() error {
	if !o.paused {
		return nil
	}
	o.deadline = time.Now().Add(o.remaining)
	o.paused = false
	for {
		err := o.query()
		if err == nil {
			return nil
		}
		remaining := time.Until(o.deadline)
		if remaining <= 0 {
			o.stop("manager observation expired after thaw", stopFailed)
			return err
		}
		timer := time.NewTimer(min(systemdObservePeriod, remaining))
		select {
		case <-timer.C:
		case <-o.client.processDone:
			timer.Stop()
			return errors.New("systemd manager exited after thaw")
		case <-o.client.link.done:
			timer.Stop()
			return errors.New("systemd executor disconnected after thaw")
		}
		if o.client.stopping.Load() {
			return errors.New("systemd environment is stopping")
		}
	}
}

func (o *systemdObserver) run() {
	defer close(o.done)
	defer o.closePeer()
	ticker := time.NewTicker(systemdObservePeriod)
	defer ticker.Stop()
	health := time.NewTimer(time.Until(o.deadline))
	defer health.Stop()
	var healthC <-chan time.Time
	linkDone := o.client.link.done
	for {
		if !health.Stop() {
			select {
			case <-health.C:
			default:
			}
		}
		healthC = nil
		if !o.paused && !o.client.stopping.Load() {
			health.Reset(time.Until(o.deadline))
			healthC = health.C
		}
		select {
		case <-o.client.processDone:
			return
		case <-linkDone:
			linkDone = nil
			o.stop("executor control EOF", stopUnverified)
			// Keep the independent manager peer for pre-poweroff inspection.
		case <-healthC:
			o.stop("manager observation expired", stopFailed)
		case cmd := <-o.commands:
			if cmd.op == observerInspectStop {
				state, err := o.queryState(cmd.ctx)
				cmd.result <- observerResult{state: state, err: err}
				continue
			}
			if o.client.stopping.Load() {
				cmd.result <- observerResult{err: errors.New("systemd environment is stopping")}
				continue
			}
			if cmd.op == observerPause {
				if !o.paused {
					o.remaining = time.Until(o.deadline)
					o.paused = true
				}
				if o.remaining <= 0 {
					o.stop("manager observation expired before freeze", stopFailed)
					cmd.result <- observerResult{err: errors.New("systemd observation expired")}
				} else {
					cmd.result <- observerResult{}
				}
			} else {
				cmd.result <- observerResult{err: o.resume()}
			}
		case <-ticker.C:
			if o.paused || o.client.stopping.Load() {
				continue
			}
			if err := o.query(); err != nil {
				logf("systemd manager observation: %v", err)
				if !time.Now().Before(o.deadline) {
					o.stop("manager observation expired", stopFailed)
				}
			}
			// There is one active attempt, with no backlog of missed ticks.
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}
