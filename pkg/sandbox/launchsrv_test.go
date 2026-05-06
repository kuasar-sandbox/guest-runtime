package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// TestLaunchServer_HelloLaunchHandshake spawns the launch server on a
// UDS and acts as a fake guest sandbox-init: connect, send hello, expect
// the launch spec back. Mirrors what cloud-hypervisor's vsock proxy does
// to sandbox-init's request.
func TestLaunchServer_HelloLaunchHandshake(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vsock.sock_5000")

	spec := &proto.LaunchSpec{
		Exec:    "/usr/bin/echo",
		Args:    []string{"hello"},
		Env:     map[string]string{"PATH": "/usr/bin"},
		Workdir: "/",
		Restart: "never",
	}
	srv := &LaunchServer{
		Path: sockPath,
		Spec: spec,
		Logf: func(string, ...any) {},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ctx) }()

	// Fake guest: connect, send hello, read launch.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := proto.WriteMessage(conn, &proto.Message{
		Type:  proto.TypeHello,
		Phase: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := proto.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != proto.TypeLaunch || got.Launch == nil {
		t.Fatalf("expected launch message, got %+v", got)
	}
	if !reflect.DeepEqual(got.Launch, spec) {
		t.Errorf("launch spec mismatch:\n got=%+v\nwant=%+v", got.Launch, spec)
	}

	// Server must close the connection immediately after sending launch
	// (single-shot handshake; see Serve docstring on snapshot cleanliness).
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := proto.ReadMessage(conn); err == nil || (!errors.Is(err, io.EOF) && err != io.ErrUnexpectedEOF) {
		t.Errorf("expected EOF after launch (server should close), got err=%v", err)
	}
	conn.Close()

	srv.Stop()
	if err := <-srvErr; err != nil {
		t.Errorf("Serve returned: %v", err)
	}
}

func TestLaunchServer_NilSpecRejected(t *testing.T) {
	srv := &LaunchServer{Path: "/tmp/never.sock"}
	if err := srv.Listen(); err == nil {
		t.Fatal("expected error when Spec is nil")
	}
}

func TestMergeLaunch_OverrideTakesPrecedence(t *testing.T) {
	image := &ImageConfig{
		Cmd:        []string{"image-arg"},
		Entrypoint: []string{"/image/exec"},
		Env:        []string{"FROM_IMAGE=1", "BOTH=image"},
		WorkingDir: "/image-dir",
	}
	override := LaunchConfig{
		Exec:    "/override/exec",
		Args:    []string{"override-arg"},
		Env:     map[string]string{"BOTH": "override", "FROM_OVERRIDE": "1"},
		Workdir: "/override-dir",
		Restart: "always",
	}
	got, err := MergeLaunch(image, override)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/override/exec" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"override-arg"}) {
		t.Errorf("Args = %v", got.Args)
	}
	if got.Workdir != "/override-dir" {
		t.Errorf("Workdir = %q", got.Workdir)
	}
	if got.Restart != "always" {
		t.Errorf("Restart = %q", got.Restart)
	}
	if got.Env["BOTH"] != "override" {
		t.Errorf("Env BOTH = %q (override should win)", got.Env["BOTH"])
	}
	if got.Env["FROM_IMAGE"] != "1" {
		t.Errorf("Env FROM_IMAGE = %q (image-only key should survive)", got.Env["FROM_IMAGE"])
	}
	if got.Env["FROM_OVERRIDE"] != "1" {
		t.Errorf("Env FROM_OVERRIDE = %q", got.Env["FROM_OVERRIDE"])
	}
}

func TestMergeLaunch_FallsBackToImageEntrypoint(t *testing.T) {
	image := &ImageConfig{
		Entrypoint: []string{"/image/exec", "ep-arg"},
		Cmd:        []string{"cmd-arg"},
	}
	override := LaunchConfig{} // empty override
	got, err := MergeLaunch(image, override)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/image/exec" {
		t.Errorf("Exec = %q", got.Exec)
	}
	wantArgs := []string{"ep-arg", "cmd-arg"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", got.Args, wantArgs)
	}
}

func TestMergeLaunch_ErrorWhenNoExecAnywhere(t *testing.T) {
	_, err := MergeLaunch(&ImageConfig{}, LaunchConfig{})
	if err == nil {
		t.Fatal("expected error when neither image nor override provides exec")
	}
}

func TestMergeLaunch_FallsBackToImageCmdWhenEntrypointEmpty(t *testing.T) {
	// python:3.12-slim shape: Entrypoint=[], Cmd=["python3"]
	image := &ImageConfig{
		Cmd: []string{"python3"},
		Env: []string{"PATH=/usr/local/bin:/usr/bin"},
	}
	got, err := MergeLaunch(image, LaunchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "python3" {
		t.Errorf("Exec = %q, want python3", got.Exec)
	}
	if len(got.Args) != 0 {
		t.Errorf("Args = %v, want empty", got.Args)
	}
}

func TestMergeLaunch_OverrideArgsReplaceImageCmd(t *testing.T) {
	// Docker semantics: `docker run img foo bar` keeps Entrypoint, replaces Cmd.
	image := &ImageConfig{
		Entrypoint: []string{"/usr/bin/wrap"},
		Cmd:        []string{"image-cmd-arg"},
	}
	got, err := MergeLaunch(image, LaunchConfig{Args: []string{"new-arg"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/usr/bin/wrap" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"new-arg"}) {
		t.Errorf("Args = %v, want [new-arg] (override replaces image.Cmd)", got.Args)
	}
}

func TestMergeLaunch_CmdOnlyImageWithOverrideArgs(t *testing.T) {
	// python:3.12-slim + override args = "python3 -c 'print(...)'"
	image := &ImageConfig{Cmd: []string{"python3"}}
	got, err := MergeLaunch(image, LaunchConfig{
		Args: []string{"-c", "print('hi')"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "python3" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"-c", "print('hi')"}) {
		t.Errorf("Args = %v", got.Args)
	}
}
