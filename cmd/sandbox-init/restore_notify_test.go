package main

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

func TestRestoreNotificationOnlySubscribedLiveMainProcesses(t *testing.T) {
	s := &supervisorState{spec: &proto.LaunchSpec{RestoreSignal: 10, StopSignal: 15}, pluginReg: newPluginRegistry()}
	s.appPid.Store(101)
	live := &pluginProc{spec: proto.PluginSpec{RestoreSignal: 12}}
	absent := &pluginProc{spec: proto.PluginSpec{RestoreSignal: 10}, pending: true}
	s.pluginReg.byPID[202] = live
	s.pluginReg.byPID[303] = &pluginProc{}
	s.pluginReg.all = []*pluginProc{live, absent}
	got := map[int]syscall.Signal{}
	s.notifyRestore(func(pid int, sig syscall.Signal) error { got[pid] = sig; return syscall.EPERM })
	if !reflect.DeepEqual(got, map[int]syscall.Signal{101: 10, 202: 12}) {
		t.Fatalf("recipients: %v", got)
	}
	s.appPid.Store(0)
	delete(s.pluginReg.byPID, 202)
	s.notifyRestore(func(pid int, sig syscall.Signal) error {
		t.Errorf("notified absent/unsubscribed pid %d", pid)
		return nil
	})
}

func TestRestoreNotificationPinsReaping(t *testing.T) {
	s := &supervisorState{spec: &proto.LaunchSpec{RestoreSignal: 10}}
	s.appPid.Store(101)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s.notifyRestore(func(int, syscall.Signal) error { close(entered); <-release; return nil })
	}()
	<-entered
	if s.primaryMu.TryLock() {
		s.primaryMu.Unlock()
		t.Error("reaper can recycle PID during delivery")
	}
	close(release)
	<-done
}

func TestLifecycleRestoreAndAttachUseDistinctPrivateOperations(t *testing.T) {
	for _, restored := range []bool{false, true} {
		outer, inner := executorPairForTest(t)
		serveExecutorForTest(t, outer, noExecutorRequests)
		serveExecutorForTest(t, inner, func(msg executorMessage, fd *int) (executorMessage, error) {
			want := "attach"
			if restored {
				want = "restore"
			}
			if msg.Op != want || *fd != -1 {
				t.Errorf("lifecycle request: %+v fd %d", msg, *fd)
			}
			return executorMessage{}, nil
		})
		c := &executorClient{link: outer, paused: true}
		if err := c.prepareReattach(restored); err != nil || c.paused {
			t.Fatalf("prepare: %v", err)
		}
	}
}

func TestPrivateRPCPropagatesBudgetAndDisconnect(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		outer, inner := executorPairForTest(t)
		entered, ended := make(chan struct{}), make(chan struct{})
		serveExecutorForTest(t, outer, noExecutorRequests)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		serveExecutorForTest(t, inner, func(msg executorMessage, _ *int) (executorMessage, error) {
			deadline, ok := msg.context().Deadline()
			want, _ := ctx.Deadline()
			if !ok || !deadline.Equal(want) {
				t.Error("server has a fresh/absent budget")
			}
			close(entered)
			<-msg.context().Done()
			close(ended)
			return executorMessage{}, msg.context().Err()
		})
		result := make(chan error, 1)
		go func() { _, err := outer.call(ctx, executorMessage{Op: "restore"}, -1); result <- err }()
		<-entered
		if cancelEarly {
			outer.close()
		}
		if err := <-result; err == nil {
			t.Fatal("uncertain operation succeeded")
		}
		waitUsageDone(t, ended)
	}
}

func restoreJobSignal(job dbus.ObjectPath, result string) *dbus.Signal {
	return &dbus.Signal{Path: "/org/freedesktop/systemd1", Name: "org.freedesktop.systemd1.Manager.JobRemoved", Body: []interface{}{uint32(42), job, systemdRestoreTarget, result}}
}

func TestRestoreTargetWaitsForThisJobResult(t *testing.T) {
	for _, result := range []string{"done", "failed", "dependency", "timeout", "canceled", "skipped"} {
		t.Run(result, func(t *testing.T) {
			ch := make(chan *dbus.Signal, 3)
			ch <- restoreJobSignal("/other", "done")
			ch <- &dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged"}
			ch <- restoreJobSignal("/job", result) // completion can precede method reply
			err := waitSystemdRestoreJob(context.Background(), ch, "/job")
			if (err == nil) != (result == "done") {
				t.Fatalf("%s: %v", result, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitSystemdRestoreJob(ctx, make(chan *dbus.Signal), "/job"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	closed := make(chan *dbus.Signal)
	close(closed)
	if err := waitSystemdRestoreJob(context.Background(), closed, "/job"); err == nil {
		t.Fatal("disconnect succeeded")
	}
}
