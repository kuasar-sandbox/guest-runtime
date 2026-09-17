package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// Each export runs in a subprocess: fatal/flag exit behavior is observable and
// mkfs/config environment changes never affect another test or the parent.
func TestExportLifecycle(t *testing.T) {
	cases := []struct {
		name, mode, output, config, wantErr, wantFile       string
		upload, stream, mkfsFail, sharedScratch, brokenPipe bool
	}{
		{name: "mkfs-partial-output", output: "file", mkfsFail: true, wantErr: "flatten: mkfs.erofs: exit status 7: fake mkfs failure", wantFile: "previous"},
		{name: "artifact-create", output: "directory", wantErr: "create artifact: open", wantFile: "previous"},
		{name: "runtime-config", output: "file", config: "runtime", wantErr: "flatten: runtime config:", wantFile: "previous"},
		{name: "missing-manifest", upload: true, wantErr: "missing manifest config: pass --manifest-config <path> or set MANIFEST_CONFIG", wantFile: "previous"},
		{name: "invalid-manifest", upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "previous"},
		{name: "unreadable-manifest", upload: true, config: "unreadable", wantErr: "load config: manifest: read", wantFile: "previous"},
		{name: "invalid-manifest-named-output", output: "file", upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "artifact"},
		{name: "invalid-ingester", upload: true, config: "empty", wantErr: "ingester: manifest: store.endpoint required for ingest", wantFile: "previous"},
		{name: "ingest-key-failure", upload: true, config: "no-key", wantErr: "ingest: customer key: manifest: customer key required", wantFile: "previous"},
		{name: "ingest-failure", mode: "upload-failure", upload: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "previous"},
		{name: "ingest-failure-named-output", mode: "upload-failure", output: "file", upload: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "artifact"},
		{name: "ingest-failure-stdout", mode: "upload-failure", output: "-", upload: true, stream: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "previous"},
		{name: "write-file", mode: "write-failure", output: "file", wantErr: "pack image artifact: injected write failure", wantFile: "partial"},
		{name: "write-temp", mode: "write-failure", upload: true, wantErr: "pack image artifact: injected write failure", wantFile: "previous"},
		{name: "close-file", mode: "close-failure", output: "file", wantErr: "close artifact: injected close failure", wantFile: "artifact"},
		{name: "close-temp", mode: "close-failure", upload: true, wantErr: "close artifact: injected close failure", wantFile: "previous"},
		{name: "write-and-close", mode: "write-close-failure", upload: true, wantErr: "pack image artifact: injected write failure", wantFile: "previous"},
		{name: "stdout-write", mode: "closed-stdout", output: "-", wantErr: "pack image artifact: write", wantFile: "previous"},
		{name: "stdout-copy", mode: "closed-stdout", output: "-", upload: true, wantErr: "write to stdout: write", wantFile: "previous"},
		{name: "broken-pipe-stream", output: "-", brokenPipe: true, wantErr: "pack image artifact:", wantFile: "previous"},
		{name: "broken-pipe-copy", output: "-", upload: true, brokenPipe: true, wantErr: "write to stdout:", wantFile: "previous"},
		{name: "broken-pipe-key", mode: "upload-success", upload: true, config: "empty", brokenPipe: true, wantErr: "write manifest key:", wantFile: "previous"},
		{name: "broken-pipe-key-named-output", mode: "upload-success", output: "file", upload: true, config: "empty", brokenPipe: true, wantErr: "write manifest key:", wantFile: "artifact"},
		{name: "terminal-output", mode: "terminal-stdout", output: "-", wantErr: "export: refusing to write an image artifact to a terminal", wantFile: "previous"},
		{name: "file-success", output: "file", wantFile: "artifact"},
		{name: "stdout-success", output: "-", stream: true, wantFile: "previous"},
		{name: "upload-success", mode: "upload-success", upload: true, config: "empty", wantFile: "previous"},
		{name: "upload-file-success", mode: "upload-success", output: "file", upload: true, config: "empty", wantFile: "artifact"},
		{name: "upload-stdout-success", mode: "upload-success", output: "-", upload: true, stream: true, config: "empty", wantFile: "previous"},
		{name: "shared-scratch", output: "scratch-file", sharedScratch: true, wantFile: "artifact"},
		{name: "shared-scratch-mkfs-failure", output: "file", sharedScratch: true, mkfsFail: true, wantErr: "flatten: mkfs.erofs: exit status 7: fake mkfs failure", wantFile: "previous"},
		{name: "shared-scratch-config-failure", output: "scratch-file", sharedScratch: true, upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "artifact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExportFixture(t)
			wantArtifact := expectedExportArtifact(t)
			args := []string{"--no-progress", "--tmpdir", f.scratch, "--config", f.config}
			output := f.output
			scratchFiles := map[string][]byte{}
			switch tc.output {
			case "file":
				args = append(args, "--output", output)
			case "scratch-file":
				output = filepath.Join(f.scratch, "user.img")
				args = append(args, "--output", output)
				scratchFiles["user.img"] = wantArtifact
			case "directory":
				args = append(args, "--output", filepath.Join(f.root, "cache"))
			case "-":
				args = append(args, "--output", "-")
			}
			if tc.upload {
				args = append(args, "--upload")
			}
			if tc.config != "" {
				path := filepath.Join(f.root, "manifest.yaml")
				contents := "{}\n"
				switch tc.config {
				case "invalid", "runtime":
					contents = "[invalid"
				case "unreadable":
					path = filepath.Join(f.root, "missing.yaml")
				case "no-key":
					// The real ingester rejects the missing key before any RPC.
					contents = fmt.Sprintf("store:\n  endpoint: %q\ncrypto:\n  chunk: aes\n  manifest: aes\n", "unix://"+filepath.Join(f.root, "no-store.sock"))
				}
				if tc.config != "unreadable" {
					writeExportFile(t, path, []byte(contents))
					f.retained[path] = []byte(contents)
				}
				flag := "--manifest-config"
				if tc.config == "runtime" {
					flag = "--runtime-config"
				}
				args = append(args, flag, path)
			}
			if tc.mkfsFail {
				writeExportFile(t, f.mkfs, []byte(fakeExportMkfs+"echo 'fake mkfs failure' >&2\nexit 7\n"))
			}
			if tc.sharedScratch {
				// Even lookalike names in shared scratch belong to someone else.
				for _, name := range []string{"flatten-erofs-user.img", "flatten-image-user.img"} {
					scratchFiles[name] = []byte("unrelated scratch\n")
					writeExportFile(t, filepath.Join(f.scratch, name), scratchFiles[name])
				}
			}
			args = append(args, f.source)
			stdout, stderr, code := f.run(t, tc.mode, tc.brokenPipe, args...)
			wantCode := 0
			if tc.wantErr != "" {
				wantCode = 1
				if !strings.HasPrefix(stderr, "error: ") || !strings.Contains(stderr, tc.wantErr) {
					t.Errorf("stderr = %q, want error containing %q", stderr, tc.wantErr)
				}
			} else if stderr != "" {
				t.Errorf("unexpected stderr: %s", stderr)
			}
			if code != wantCode {
				t.Errorf("exit = %d, want %d; stderr: %s", code, wantCode, stderr)
			}
			if tc.brokenPipe && !strings.Contains(stderr, "broken pipe") {
				t.Errorf("stderr = %q, want broken pipe diagnostic", stderr)
			}
			var wantStdout []byte
			if tc.stream {
				wantStdout = append(wantStdout, wantArtifact...)
			}
			if tc.mode == "upload-success" && !tc.brokenPipe {
				wantStdout = append(wantStdout, strings.Repeat("a", 64)+"\n"...)
			}
			if !bytes.Equal(stdout, wantStdout) {
				t.Errorf("stdout differs: got %d bytes, want %d", len(stdout), len(wantStdout))
			}
			wantFile := []byte("previous user artifact\n")
			switch tc.wantFile {
			case "artifact":
				wantFile = wantArtifact
			case "partial":
				wantFile = wantArtifact[:17]
			}
			assertExportFile(t, output, wantFile)
			if output != f.output {
				assertExportFile(t, f.output, []byte("previous user artifact\n"))
			}
			for path, data := range f.retained {
				assertExportFile(t, path, data)
			}
			if link, err := os.Readlink(filepath.Join(f.source, "link")); err != nil || link != "input" {
				t.Errorf("source symlink changed: %q, %v", link, err)
			}
			assertExportScratch(t, f.scratch, scratchFiles)
			if strings.HasPrefix(tc.mode, "upload-") {
				assertExportFile(t, filepath.Join(f.root, "ingested.img"), wantArtifact)
			}
			if strings.Contains(tc.mode, "failure") && tc.mode != "upload-failure" {
				assertExportFile(t, filepath.Join(f.root, "writer-closed"), []byte("closed\n"))
			}
		})
	}
}

func TestExportFlagExit(t *testing.T) {
	for _, tc := range []struct {
		arg, message string
		code         int
	}{
		{"--help", "Usage of export:", 0},
		{"--unknown-flag", "flag provided but not defined", 2},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			t.Parallel()
			f := newExportFixture(t)
			_, stderr, code := f.run(t, "", false, tc.arg)
			if code != tc.code || !strings.Contains(stderr, tc.message) {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
			assertExportScratch(t, f.scratch, nil)
		})
	}
}

func TestExportRestoresSIGPIPE(t *testing.T) {
	for _, outcome := range []string{"success", "error"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newExportFixture(t)
			output := f.output
			wantFile := expectedExportArtifact(t)
			if outcome == "error" {
				output = f.source // Artifact creation fails after raw output exists.
				wantFile = []byte("previous user artifact\n")
			}
			_, stderr, code := f.run(t, "restore-sigpipe-"+outcome, true,
				"--no-progress", "--tmpdir", f.scratch, "--config", f.config, "--output", output, f.source)
			// The worker has returned and cleaned up before the helper writes to
			// the broken pipe. That later write must have normal SIGPIPE behavior.
			if code != -int(syscall.SIGPIPE) || stderr != "" {
				t.Errorf("exit = %d, want SIGPIPE; stderr: %s", code, stderr)
			}
			assertExportFile(t, filepath.Join(f.root, "export-returned"), []byte("returned\n"))
			assertExportFile(t, f.output, wantFile)
			for path, data := range f.retained {
				assertExportFile(t, path, data)
			}
			assertExportScratch(t, f.scratch, nil)
		})
	}
}

// TestExportProcess is an entry point in the test executable, not a second
// implementation of export. Normal cases exercise cmdExport including fatal.
func TestExportProcess(t *testing.T) {
	i := slices.Index(os.Args, "--export-helper")
	if i < 0 {
		return
	}
	mode, root := os.Args[i+1], os.Args[i+2]
	args := os.Args[i+3:]
	if strings.HasPrefix(mode, "restore-sigpipe-") {
		err := runExport(args, exportIO{})
		if (err == nil) != (mode == "restore-sigpipe-success") {
			t.Fatalf("unexpected export result: %v", err)
		}
		writeExportFile(t, filepath.Join(root, "export-returned"), []byte("returned\n"))
		fmt.Fprintln(os.Stdout, "after export")
		t.Fatal("write to broken stdout survived after export returned")
	}
	if mode == "" || mode == "closed-stdout" || mode == "terminal-stdout" {
		if mode == "closed-stdout" {
			if err := os.Stdout.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "terminal-stdout" {
			f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = f // A character device exercises the terminal guard.
		}
		cmdExport(args)
	} else {
		var writer *exportTestWriter
		ops := exportIO{
			createArtifact: func(path string) (io.WriteCloser, error) {
				f, err := os.Create(path)
				if err != nil {
					return nil, err
				}
				writer = &exportTestWriter{File: f, mode: mode, root: root}
				return writer, nil
			},
			ingestArtifact: func(path string, _ *manifest.Config, _ bool) (string, error) {
				if writer == nil || !writer.closed {
					return "", errors.New("test: artifact writer still open at ingest")
				}
				paths, err := filepath.Glob(filepath.Join(root, "scratch", "flatten-erofs-*.img"))
				if err != nil || len(paths) != 0 {
					return "", fmt.Errorf("test: raw EROFS still present at ingest: %v, %v", paths, err)
				}
				if err := checkExportRawClosed(filepath.Join(root, "scratch")); err != nil {
					return "", err
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				if err := os.WriteFile(filepath.Join(root, "ingested.img"), b, 0o600); err != nil {
					return "", err
				}
				if mode == "upload-failure" {
					return "", errors.New("ingest: injected failure")
				}
				return strings.Repeat("a", 64), nil
			},
		}
		if err := runExport(args, ops); err != nil {
			fatal("%v", err)
		}
	}
	os.Exit(0) // Keep testing's PASS banner out of artifact stdout.
}

func checkExportRawClosed(scratch string) error {
	if runtime.GOOS != "linux" {
		return nil // The descriptor check uses Linux procfs; unlink is checked everywhere.
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		// Linux permits unlink while open; that would retain the raw disk space
		// until the descriptor closes, despite an empty scratch listing.
		if strings.HasPrefix(target, filepath.Join(scratch, "flatten-erofs-")) {
			return fmt.Errorf("test: raw EROFS still open at ingest: %s", target)
		}
	}
	return nil
}

type exportTestWriter struct {
	*os.File
	mode, root string
	closed     bool
}

func (w *exportTestWriter) Write(p []byte) (int, error) {
	if w.mode == "write-failure" || w.mode == "write-close-failure" {
		n, err := w.File.Write(p[:min(len(p), 17)])
		if err != nil {
			return n, err
		}
		return n, errors.New("injected write failure")
	}
	return w.File.Write(p)
}

func (w *exportTestWriter) Close() error {
	w.closed = true
	if err := w.File.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.root, "writer-closed"), []byte("closed\n"), 0o600); err != nil {
		return err
	}
	if w.mode == "close-failure" || w.mode == "write-close-failure" {
		return errors.New("injected close failure")
	}
	return nil
}

const fakeExportMkfs = `#!/bin/sh
set -eu
while [ "$#" -gt 2 ]; do shift; done
printf 'fake EROFS bytes\000tail\n' > "$1"
`

type exportFixture struct {
	root, source, scratch, output, config, mkfs string
	retained                                    map[string][]byte
}

func newExportFixture(t *testing.T) exportFixture {
	t.Helper()
	root := t.TempDir()
	f := exportFixture{
		root: root, source: filepath.Join(root, "source"), scratch: filepath.Join(root, "scratch"),
		output: filepath.Join(root, "user.img"), config: filepath.Join(root, "flatten.yaml"),
		mkfs: filepath.Join(root, "mkfs.erofs"), retained: make(map[string][]byte),
	}
	for _, path := range []string{f.source, f.scratch, filepath.Join(root, "cache")} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.retained[filepath.Join(root, "sentinel")] = []byte("unrelated file\n")
	f.retained[filepath.Join(root, "cache", "sentinel")] = []byte("persistent cache\n")
	f.retained[filepath.Join(f.source, "input")] = []byte("source contents\x00retained\n")
	f.retained[filepath.Join(root, "input.tar")] = []byte("unrelated user archive\n")
	f.retained[f.config] = []byte(fmt.Sprintf("cache:\n  dir: %q\n", filepath.Join(root, "cache")))
	for path, data := range f.retained {
		writeExportFile(t, path, data)
	}
	if err := os.Symlink("input", filepath.Join(f.source, "link")); err != nil {
		t.Fatal(err)
	}
	writeExportFile(t, f.output, []byte("previous user artifact\n"))
	writeExportFile(t, f.mkfs, []byte(fakeExportMkfs))
	if err := os.Chmod(f.mkfs, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f exportFixture) run(t *testing.T, mode string, brokenPipe bool, args ...string) ([]byte, string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"-test.run=^TestExportProcess$", "--", "--export-helper", mode, f.root}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.scratch, "MKFS_EROFS_PATH=" + f.mkfs, "GOMAXPROCS=2"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if brokenPipe {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		// Close the reader before launch so the child's first stdout write
		// reliably gets EPIPE; no scheduling, sleeps or injected signals.
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = writer
	}
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	code := cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = -int(status.Signal())
	}
	return stdout.Bytes(), stderr.String(), code
}

func expectedExportArtifact(t *testing.T) []byte {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "expected.raw")
	writeExportFile(t, raw, []byte("fake EROFS bytes\x00tail\n"))
	if err := image.AppendConfigZip(raw, &image.RuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	src, err := sparse.NewSource(bytes.NewReader(payload), uint64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var artifact bytes.Buffer
	if _, _, err := tarstream.WriteTo(context.Background(), &artifact, "image", src); err != nil {
		t.Fatal(err)
	}
	return artifact.Bytes()
}

func writeExportFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertExportFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("file %s: got %d bytes, want %d; err=%v; bytes equal=%v", path, len(got), len(want), err, bytes.Equal(got, want))
	}
}

func assertExportScratch(t *testing.T, path string, want map[string][]byte) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("scratch has %d entries, want %d: %v", len(entries), len(want), entries)
	}
	for _, entry := range entries {
		data, ok := want[entry.Name()]
		if !ok {
			t.Errorf("scratch leftover: %s", entry.Name())
			continue
		}
		assertExportFile(t, filepath.Join(path, entry.Name()), data)
	}
}
