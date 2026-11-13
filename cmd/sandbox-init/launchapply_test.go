package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

func TestInitUsesConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	const name = "init-path-test"
	executable := filepath.Join(dir, name)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s' \"$VALUE\" > result\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	spec := proto.InitSpec{Exec: name, Workdir: dir, Env: map[string]string{"PATH": dir, "VALUE": "child"}}
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "result")); err != nil || string(data) != "child" {
		t.Fatalf("result = %q, %v", data, err)
	}
	spec.Exec = "true"
	spec.Env = map[string]string{"VALUE": "keep-default-path"}
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatalf("default PATH: %v", err)
	}
	t.Setenv("PATH", dir)
	spec.Exec = name
	spec.Env["PATH"] = ""
	if err := runInit([]proto.InitSpec{spec}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("explicit empty PATH: %v", err)
	}
	spec.Exec = "./" + name
	if err := runInit([]proto.InitSpec{spec}); err != nil {
		t.Fatalf("explicit relative executable: %v", err)
	}
	spec.Exec = name
	spec.Env["PATH"] = "."
	if err := runInit([]proto.InitSpec{spec}); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("implicit relative executable: %v", err)
	}
}
