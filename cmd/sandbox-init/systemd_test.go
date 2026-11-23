package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

func TestSystemdSelectionDoesNotHideBrokenCandidates(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent-systemd")
	if path, err := findSystemdCandidates([]string{missing}); err != nil || path != "" {
		t.Fatalf("absent candidate: %q %v", path, err)
	}
	broken := filepath.Join(dir, "broken-systemd")
	if err := os.Symlink(missing, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := findSystemdCandidates([]string{broken}); err == nil {
		t.Fatal("broken candidate was treated as absent")
	}
	bad := filepath.Join(dir, "text-systemd")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findSystemdCandidates([]string{bad}); err == nil {
		t.Fatal("non-ELF candidate was accepted")
	}
	other := filepath.Join(dir, "openrc")
	if err := os.WriteFile(other, []byte("other init"), 0o755); err != nil {
		t.Fatal(err)
	}
	init := filepath.Join(dir, "init")
	if err := os.Symlink(other, init); err != nil {
		t.Fatal(err)
	}
	if path, err := findSystemdCandidates([]string{init}); err != nil || path != "" {
		t.Fatalf("another init is not a manager: %q %v", path, err)
	}
}

func TestSystemdManagerIdentityDeduplication(t *testing.T) {
	dir := t.TempDir()
	b, _, _ := systemdGlibcFixture(t)
	manager := filepath.Join(dir, "systemd")
	if err := os.WriteFile(manager, b, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Link(manager, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := findSystemdCandidates([]string{manager, alias}); err != nil {
		t.Fatalf("same inode rejected: %v", err)
	}
	other := filepath.Join(dir, "different-systemd")
	if err := os.WriteFile(other, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findSystemdCandidates([]string{manager, other}); err == nil {
		t.Fatal("distinct managers silently selected")
	}
}

func TestSystemdModeAndRawDirectPolicy(t *testing.T) {
	for _, mode := range []string{"", "off", "auto", "runtime", "provided"} {
		if err := validateSystemdMode(mode); err != nil {
			t.Fatal(err)
		}
		spec := &proto.LaunchSpec{Systemd: mode, CgroupControl: false, SharePID: false}
		_ = systemdEnabled(spec)
		if spec.CgroupControl || spec.SharePID {
			t.Fatal("effective mode rewrote direct policy")
		}
	}
	for _, mode := range []string{"disabled", "false", "true", "AUTO", "unknown"} {
		if err := validateSystemdMode(mode); err == nil {
			t.Fatalf("accepted invalid mode %q", mode)
		}
	}
	for seconds, want := range map[int]time.Duration{-1: 10 * time.Second, 0: 10 * time.Second, 1: time.Second, 45: 45 * time.Second} {
		if got := systemdGrace(&proto.LaunchSpec{StopGraceSec: seconds}); got != want {
			t.Fatalf("wire grace %d: %s", seconds, got)
		}
	}
}

func TestSystemdMountNames(t *testing.T) {
	for path, want := range map[string]string{"/": "-.mount", "/run": "run.mount", "/run/sandbox-runtime/systemd": "run-sandbox\\x2druntime-systemd.mount", "/a-b/c.d": "a\\x2db-c.d.mount", "/.hidden": "\\x2ehidden.mount"} {
		if got := systemdUnitName(path); got != want {
			t.Fatalf("%s: got %s want %s", path, got, want)
		}
	}
	for _, path := range []string{"/proc", "/sys", "/dev", "/run", "/sys/fs/cgroup", "/dev/console"} {
		if !systemdAPIMount(path) {
			t.Fatalf("API mount must not receive an invalid .mount unit: %s", path)
		}
	}
	for _, path := range []string{systemdPrefix, "/opt/sandbox-runtime", "/opt"} {
		if systemdAPIMount(path) {
			t.Fatalf("ordinary platform mount needs lifetime protection: %s", path)
		}
	}
}

func TestSystemdToolsDefaultPreservesExplicitPath(t *testing.T) {
	previous := runtimeSystemdTools
	runtimeSystemdTools = true
	t.Cleanup(func() { runtimeSystemdTools = previous })
	if !strings.Contains(strings.Join(execEnv(nil), "\n"), "PATH="+systemdPrefix+"/bin:") {
		t.Fatal("Runtime tools missing from default environment")
	}
	if got := testEnvMap(execEnv(map[string]string{"PATH": "/chosen"}))["PATH"]; got != "/chosen" {
		t.Fatalf("explicit PATH rewritten: %s", got)
	}
}

func TestSystemdRefreshTimeoutKeepsExecutorLinkAndClosesLateFD(t *testing.T) {
	outer, file, err := newExecutorPair()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := executorLinkFromFile(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer outer.close()
	defer inner.close()
	client := &executorClient{link: outer, systemd: &systemdEnvironment{refresh: map[uint64]chan int{}}}
	late := make(chan int, 1)
	go outer.serve(func(msg executorMessage, fd *int) (executorMessage, error) {
		if msg.Op == "systemd-peer-fd" {
			late <- *fd
		}
		return client.handleEvent(msg, fd)
	})
	release := make(chan struct{})
	go inner.serve(func(msg executorMessage, fd *int) (executorMessage, error) {
		if msg.Op != "systemd-peer" {
			return executorMessage{PID: 7}, nil
		}
		<-release
		f, err := os.Open("/dev/null")
		if err != nil {
			return executorMessage{}, err
		}
		defer f.Close()
		return executorMessage{}, inner.send(executorMessage{Op: "systemd-peer-fd", Epoch: msg.Epoch}, int(f.Fd()))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.refreshSystemdPeer(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh: %v", err)
	}
	close(release)
	var received int
	select {
	case received = <-late:
	case <-time.After(time.Second):
		t.Fatal("late FD not delivered")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if reply, err := outer.call(ctx2, executorMessage{Op: "ping"}, -1); err != nil || reply.PID != 7 {
		t.Fatalf("refresh timeout killed execution channel: %+v %v", reply, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := unix.FcntlInt(uintptr(received), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late manager descriptor leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShutdownCommitsWhenAcknowledgementIsLost(t *testing.T) {
	guest, host := usageSocketPair(t)
	defer guest.Close()
	if err := proto.WriteMessage(host, &proto.Message{Type: proto.TypeShutdown}); err != nil {
		t.Fatal(err)
	}
	_ = host.Close()
	stopped := false
	handleReverseConnShutdown(guest, nil, func() { stopped = true })
	if !stopped {
		t.Fatal("lost acknowledgement canceled shutdown")
	}
}

func TestSystemdObserverPausePreservesRemainingHealthBudget(t *testing.T) {
	client := &executorClient{link: &executorLink{done: make(chan struct{})}, processDone: make(chan struct{})}
	o := &systemdObserver{client: client, commands: make(chan observerCommand), done: make(chan struct{}), deadline: time.Now().Add(50 * time.Millisecond)}
	o.start()
	if err := o.request(true); err != nil {
		t.Fatal(err)
	}
	remaining := o.remaining
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("remaining budget %s", remaining)
	}
	time.Sleep(80 * time.Millisecond)
	if err := o.request(true); err != nil {
		t.Fatal(err)
	}
	if o.remaining != remaining {
		t.Fatalf("repeated freeze reset budget: %s -> %s", remaining, o.remaining)
	}
	close(client.processDone)
	<-o.done
}

func TestEnvironmentExitCannotHideFaultBehindPoweroff(t *testing.T) {
	for _, tc := range []struct {
		name         string
		proof        environmentStopProof
		requested    bool
		code, signal int
		state        string
	}{
		{"failed-clean-manager", stopFailed, true, 0, 0, proto.EnvironmentFailed},
		{"failed-namespace-poweroff", stopFailed, false, -1, int(unix.SIGINT), proto.EnvironmentFailed},
		{"host-request", stopNormal, true, -1, int(unix.SIGINT), proto.EnvironmentStopped},
		{"requested-escalation", stopNormal, true, -1, int(unix.SIGKILL), proto.EnvironmentFailed},
		{"voluntary-fast-poweroff", stopUnverified, false, -1, int(unix.SIGINT), proto.EnvironmentStopped},
		{"induced-poweroff-is-not-proof", stopUnverified, true, -1, int(unix.SIGINT), proto.EnvironmentFailed},
		{"unexplained-kill", stopUnverified, false, -1, int(unix.SIGKILL), proto.EnvironmentFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEnvironmentExit(environmentStopFact{reason: "original cause", proof: tc.proof, poweroffRequested: tc.requested}, tc.code, tc.signal)
			if got.State != tc.state || got.Code != tc.code || got.TermSignal != tc.signal || got.Reason != "original cause" {
				t.Fatalf("outcome lost independent facts: %+v", got)
			}
		})
	}
}

func TestEnvironmentExitAcknowledgementHasOneDeadline(t *testing.T) {
	guest, host := usageSocketPair(t)
	defer guest.Close()
	defer host.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := proto.ReadMessage(host)
		if err != nil || req.Type != proto.TypeEnvironmentExited || req.EnvironmentExit == nil {
			return
		}
		// An ACK that makes partial progress cannot restart the total budget.
		for _, b := range []byte{16, 0, 0, 0, '{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 'a', 'c', 'k', '"', '}'} {
			if _, err := host.Write([]byte{b}); err != nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()
	start := time.Now()
	err := exchangeEnvironmentExit(guest, proto.EnvironmentExit{State: proto.EnvironmentFailed, Reason: "observer expiry"}, start.Add(45*time.Millisecond))
	if err == nil {
		t.Fatal("partial acknowledgement escaped deadline")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("notification exceeded one deadline: %s", elapsed)
	}
	_ = host.Close()
	<-done
}

func TestSystemdObserverExpiresBetweenPollingTicks(t *testing.T) {
	client := &executorClient{link: &executorLink{done: make(chan struct{})}, processDone: make(chan struct{})}
	stopped := make(chan environmentStopProof, 1)
	start := time.Now()
	o := &systemdObserver{client: client, commands: make(chan observerCommand), done: make(chan struct{}), deadline: start.Add(40 * time.Millisecond), stop: func(_ string, p environmentStopProof) { client.stopping.Store(true); stopped <- p }}
	o.start()
	defer func() { close(client.processDone); <-o.done }()
	select {
	case proof := <-stopped:
		if proof != stopFailed {
			t.Fatalf("expiry proof=%v", proof)
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Fatalf("expiry waited for a poll tick: %s", elapsed)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("health expiry depends on one-second polling ticker")
	}
}

func TestEnvironmentEscalationKeepsFailureDespiteCleanConcurrentExit(t *testing.T) {
	result := classifyEnvironmentExit(environmentStopFact{reason: "Host shutdown", proof: stopNormal, poweroffRequested: true, forced: true}, 0, 0)
	if result.State != proto.EnvironmentFailed {
		t.Fatalf("forced stop lost: %+v", result)
	}
}

func testEnvMap(env []string) map[string]string {
	m := map[string]string{}
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			m[k] = v
		}
	}
	return m
}

func TestSystemdLoginPathUsesProvenanceNotValue(t *testing.T) {
	previous := runtimeSystemdTools
	runtimeSystemdTools = true
	t.Cleanup(func() { runtimeSystemdTools = previous })
	for _, tc := range []struct {
		name, path string
		trusted    bool
		marker     string
	}{
		{"explicit-default-looking", executionDefaultPath(), false, "0"},
		{"explicit-empty", "", false, "0"},
		{"product-default", "/custom/image/bin:/bin", true, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnvMap(execEnvWithPathSource(map[string]string{"PATH": tc.path, systemdPathMarker: "1"}, tc.trusted))
			want := tc.path
			if tc.trusted {
				want = prependSystemdTools(want)
			}
			if env["PATH"] != want || env[systemdPathMarker] != tc.marker {
				t.Fatalf("lost PATH authority: %+v", env)
			}
		})
	}
}

func TestSystemdStandardLoginProfilePreservesExplicitChoice(t *testing.T) {
	prefix := t.TempDir()
	if err := os.Mkdir(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "profile.sh")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(systemdLoginScript(), systemdPrefix, prefix)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ marker, path, want string }{
		{"1", "/image/bin:/bin", prefix + "/bin:/image/bin:/bin"},
		{"0", "/chosen", "/chosen"},
		{"0", "", ""},
		{"1", prefix + "/bin:/bin", prefix + "/bin:/bin"},
	} {
		cmd := exec.Command("/bin/sh", "-c", `. "$1"; printf '%s' "$PATH"`, "sh", script)
		cmd.Env = []string{"PATH=" + tc.path, systemdPathMarker + "=" + tc.marker}
		got, err := cmd.Output()
		if err != nil || string(got) != tc.want {
			t.Fatalf("marker=%s path=%q: got %q err=%v", tc.marker, tc.path, got, err)
		}
	}
}
