package stdio

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func ptr(b bool) *bool { return &b }

func TestFromFlags_Default(t *testing.T) {
	m, err := FromFlags(nil, nil, nil, "", "", "", false)
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if m.TTY {
		t.Fatal("TTY=true unexpectedly")
	}
	if m.Stdin.Kind != StreamDevNull {
		t.Errorf("default stdin = %v, want DevNull", m.Stdin.Kind)
	}
	if m.Stdout.Kind != StreamInherit {
		t.Errorf("default stdout = %v, want Inherit", m.Stdout.Kind)
	}
	if m.Stderr.Kind != StreamInherit {
		t.Errorf("default stderr = %v, want Inherit", m.Stderr.Kind)
	}
}

func TestFromFlags_StdinEnabled(t *testing.T) {
	m, err := FromFlags(ptr(true), nil, nil, "", "", "", false)
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if m.Stdin.Kind != StreamInherit {
		t.Errorf("--stdin: stdin = %v, want Inherit", m.Stdin.Kind)
	}
}

func TestFromFlags_FileRedirects(t *testing.T) {
	m, err := FromFlags(nil, nil, nil, "/tmp/in", "/tmp/out", "/tmp/err", false)
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if m.Stdin.Kind != StreamFile || m.Stdin.Path != "/tmp/in" {
		t.Errorf("stdin: %+v", m.Stdin)
	}
	if m.Stdout.Kind != StreamFile || m.Stdout.Path != "/tmp/out" {
		t.Errorf("stdout: %+v", m.Stdout)
	}
	if m.Stderr.Kind != StreamFile || m.Stderr.Path != "/tmp/err" {
		t.Errorf("stderr: %+v", m.Stderr)
	}
}

func TestFromFlags_TTYConflict(t *testing.T) {
	cases := []struct {
		name string
		stdin, stdout, stderr *bool
		from, to1, to2 string
	}{
		{"--tty + --stdin", ptr(true), nil, nil, "", "", ""},
		{"--tty + --stdout=false", nil, ptr(false), nil, "", "", ""},
		{"--tty + --stdin-from", nil, nil, nil, "/tmp/x", "", ""},
		{"--tty + --stdout-to", nil, nil, nil, "", "/tmp/x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromFlags(tc.stdin, tc.stdout, tc.stderr, tc.from, tc.to1, tc.to2, true)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestFromFlags_FalseConflicts(t *testing.T) {
	if _, err := FromFlags(ptr(false), nil, nil, "/tmp/x", "", "", false); err == nil {
		t.Error("--stdin=false + --stdin-from should conflict")
	}
	if _, err := FromFlags(nil, ptr(false), nil, "", "/tmp/x", "", false); err == nil {
		t.Error("--stdout=false + --stdout-to should conflict")
	}
	if _, err := FromFlags(nil, nil, ptr(false), "", "", "/tmp/x", false); err == nil {
		t.Error("--stderr=false + --stderr-to should conflict")
	}
}

func TestApply_DevNullStdin_FileStdout(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.log")
	mode := Mode{
		Stdin:  Stream{Kind: StreamDevNull},
		Stdout: Stream{Kind: StreamFile, Path: out},
		Stderr: Stream{Kind: StreamDevNull},
	}
	cmd := exec.Command("/bin/sh", "-c", "echo hello-stdio")
	cleanup, err := mode.Apply(cmd)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	defer cleanup()
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if string(got) != "hello-stdio\n" {
		t.Fatalf("got %q, want %q", string(got), "hello-stdio\n")
	}
}
