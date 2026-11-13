package main

import (
	"sync"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// The initial startup fact is independent of supervisor replacement and PID
// changes. Ping reads one small copy; it never waits for app output or storage.
type initialLaunchOwner struct {
	mu    sync.Mutex
	state proto.InitialLaunchState
}

var initialLaunch initialLaunchOwner

func (o *initialLaunchOwner) begin(token string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// One Guest accepts one initial workload. A shutdown can precede token
	// binding; binding must preserve that terminal fact.
	if o.state.Token != "" {
		return
	}
	o.state.Token = token
	if o.state.State == "" {
		o.state.State = proto.InitialLaunchPending
	}
}

func (o *initialLaunchOwner) started(pid int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.State == proto.InitialLaunchPending {
		o.state.State, o.state.PID = proto.InitialLaunchStarted, pid
	}
}

func (o *initialLaunchOwner) failed(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.State == proto.InitialLaunchPending {
		o.state.State = proto.InitialLaunchFailed
		o.state.Error = err.Error()
	}
}

func (o *initialLaunchOwner) exited(code int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.State == "" || o.state.State == proto.InitialLaunchPending || o.state.State == proto.InitialLaunchStarted {
		o.state.State, o.state.Code = proto.InitialLaunchExited, code
	}
}

func (o *initialLaunchOwner) snapshot() *proto.InitialLaunchState {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.Token == "" {
		return nil // old Hosts did not request initial state
	}
	state := o.state
	return &state
}
