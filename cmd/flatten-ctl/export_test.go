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
	"time"

	"golang.org/x/sys/unix"

	"github.com/kuasar-sandbox/accelerator/pkg/flatten"
	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// Each export runs in a subprocess: fatal/flag exit behavior is observable and
// generator/config environment changes never affect another test or the parent.
func TestExportLifecycle(t *testing.T) {
	cases := []struct {
		name, mode, output, config, wantErr, wantFile string
		upload, stream, sharedScratch, brokenPipe     bool
	}{
		{name: "artifact-create", output: "directory", wantErr: "create artifact:", wantFile: "previous"},
		{name: "runtime-config", output: "file", config: "runtime", wantErr: "invalid character", wantFile: "previous"},
		{name: "missing-manifest", upload: true, wantErr: "missing manifest config: pass --manifest-config <path> or set MANIFEST_CONFIG", wantFile: "previous"},
		{name: "invalid-manifest", upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "previous"},
		{name: "unreadable-manifest", upload: true, config: "unreadable", wantErr: "load config: manifest: read", wantFile: "previous"},
		{name: "invalid-manifest-named-output", output: "file", upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "previous"},
		{name: "invalid-ingester", upload: true, config: "empty", wantErr: "ingester: manifest: store.endpoint required for ingest", wantFile: "previous"},
		{name: "ingest-key-failure", upload: true, config: "no-key", wantErr: "ingest: customer key: manifest: customer key required", wantFile: "previous"},
		{name: "ingest-failure", mode: "upload-failure", upload: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "previous"},
		{name: "ingest-failure-named-output", mode: "upload-failure", output: "file", upload: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "previous"},
		{name: "ingest-failure-stdout", mode: "upload-failure", output: "-", upload: true, stream: true, config: "empty", wantErr: "ingest: injected failure", wantFile: "previous"},
		{name: "write-file", mode: "write-failure", output: "file", wantErr: "pack image artifact: injected write failure", wantFile: "previous"},
		{name: "write-temp", mode: "write-failure", upload: true, config: "empty", wantErr: "pack image artifact: injected write failure", wantFile: "previous"},
		{name: "close-file", mode: "close-failure", output: "file", wantErr: "close artifact: injected close failure", wantFile: "previous"},
		{name: "close-temp", mode: "close-failure", upload: true, config: "empty", wantErr: "close artifact: injected close failure", wantFile: "previous"},
		{name: "write-and-close", mode: "write-close-failure", upload: true, config: "empty", wantErr: "pack image artifact: injected write failure", wantFile: "previous"},
		{name: "stdout-write", mode: "closed-stdout", output: "-", wantErr: "pack image artifact: write", wantFile: "previous"},
		{name: "stdout-copy", mode: "closed-stdout", output: "-", upload: true, config: "empty", wantErr: "write to stdout: write", wantFile: "previous"},
		{name: "broken-pipe-stream", output: "-", brokenPipe: true, wantErr: "pack image artifact:", wantFile: "previous"},
		{name: "broken-pipe-copy", output: "-", upload: true, config: "empty", brokenPipe: true, wantErr: "write to stdout:", wantFile: "previous"},
		{name: "broken-pipe-key", mode: "upload-success", upload: true, config: "empty", brokenPipe: true, wantErr: "write manifest key:", wantFile: "previous"},
		{name: "broken-pipe-key-named-output", mode: "upload-success", output: "file", upload: true, config: "empty", brokenPipe: true, wantErr: "write manifest key:", wantFile: "artifact"},
		{name: "terminal-output", mode: "terminal-stdout", output: "-", wantErr: "export: refusing to write an image artifact to a terminal", wantFile: "previous"},
		{name: "file-success", output: "file", wantFile: "artifact"},
		{name: "stdout-success", output: "-", stream: true, wantFile: "previous"},
		{name: "upload-success", mode: "upload-success", upload: true, config: "empty", wantFile: "previous"},
		{name: "upload-file-success", mode: "upload-success", output: "file", upload: true, config: "empty", wantFile: "artifact"},
		{name: "upload-stdout-success", mode: "upload-success", output: "-", upload: true, stream: true, config: "empty", wantFile: "previous"},
		{name: "shared-scratch", output: "scratch-file", sharedScratch: true, wantFile: "artifact"},
		{name: "shared-scratch-config-failure", output: "scratch-file", sharedScratch: true, upload: true, config: "invalid", wantErr: "load config: manifest: parse", wantFile: "previous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newExportFixture(t)
			wantArtifact := expectedExportArtifact(t, f)
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
				writeExportFile(t, output, []byte("previous user artifact\n"))
				if tc.wantFile == "previous" {
					scratchFiles["user.img"] = []byte("previous user artifact\n")
				}
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
			wantFile := expectedExportArtifact(t, f)
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
	if mode == "umask-0077" {
		syscall.Umask(0077)
		mode = ""
	}
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
			createArtifact: func(file *os.File) (io.WriteCloser, error) {
				owned, err := artifactWriter(file)
				if err != nil {
					return nil, err
				}
				writer = &exportTestWriter{File: owned.(*os.File), mode: mode, root: root}
				return writer, nil
			},
			ingestArtifact: func(_ context.Context, file *os.File, _ *manifest.Config, _ bool) (string, error) {
				if writer == nil || !writer.closed {
					return "", errors.New("test: artifact writer still open at ingest")
				}
				paths, err := filepath.Glob(filepath.Join(root, "scratch", "flatten-prepared-*"))
				if err != nil || len(paths) != 0 {
					return "", fmt.Errorf("test: raw EROFS still present at ingest: %v, %v", paths, err)
				}
				if err := checkExportRawClosed(filepath.Join(root, "scratch")); err != nil {
					return "", err
				}
				info, err := file.Stat()
				if err != nil {
					return "", err
				}
				b, err := io.ReadAll(io.NewSectionReader(file, 0, info.Size()))
				if err != nil {
					return "", err
				}
				if err := os.WriteFile(filepath.Join(root, "ingested.img"), b, 0o600); err != nil {
					return "", err
				}
				if mode == "upload-destination-fifo" {
					output := filepath.Join(root, "result.img")
					if err := os.Remove(output); err != nil {
						return "", err
					}
					if err := syscall.Mkfifo(output, 0600); err != nil {
						return "", err
					}
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
		if strings.HasPrefix(target, filepath.Join(scratch, "flatten-prepared-")) {
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
	// A stale environment variable must never select an external generator.
	writeExportFile(t, f.mkfs, []byte("#!/bin/sh\necho unexpected native generator >&2\nexit 77\n"))
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

func expectedExportArtifact(t *testing.T, f exportFixture) []byte {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "expected.raw")
	// Packaging must agree with the explicit raw API of the same generator.
	// Independent filesystem semantics are checked by accelerator's reader,
	// native fsck and kernel mount tests, not by comparing native layout bytes.
	if err := flatten.BuildFromDir(f.source, raw, flatten.Options{TmpDir: f.scratch}, flatten.DirOptions{}); err != nil {
		t.Fatal(err)
	}
	var artifact bytes.Buffer
	if err := packImageArtifact(raw, &artifact); err != nil {
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

// Expected envelopes use the independently known raw payload length.
type exportBoundarySource struct {
	sparse.Source
	boundary uint64
}

func (s exportBoundarySource) PayloadCommitment() (uint64, [32]byte, bool) {
	return s.boundary, [32]byte{}, false
}
func (s exportBoundarySource) TarStreamDigest(string) ([32]byte, bool) { return [32]byte{}, false }

func TestPackedImageDeclaresOriginalPayloadBoundary(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "image.raw")
	payload := []byte("bounded image payload")
	writeExportFile(t, raw, payload)
	if err := image.AppendConfigZip(raw, &image.RuntimeConfig{Cmd: []string{"/bin/test"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := packImageArtifact(raw, &out); err != nil {
		t.Fatal(err)
	}
	source, _, err := tarstream.SourceAt(bytes.NewReader(out.Bytes()), int64(out.Len()), "image")
	if err != nil {
		t.Fatal(err)
	}
	boundary, _, ok := source.(tarstream.IdentityProvider).PayloadCommitment()
	if !ok || boundary != uint64(len(payload)) {
		t.Fatalf("payload boundary=%d valid=%v", boundary, ok)
	}
	all := make([]byte, source.Size())
	if _, err = source.ReadAt(context.Background(), all, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all[:len(payload)], payload) {
		t.Fatal("payload changed")
	}
}

func TestExportNamedOutputPermissions(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, mode := range []string{"", "umask-0077"} {
			t.Run(fmt.Sprintf("existing-%t/%s", existing, mode), func(t *testing.T) {
				f := newExportFixture(t)
				var want os.FileMode
				if existing {
					if err := os.Chmod(f.output, 0660); err != nil {
						t.Fatal(err)
					}
					want = 0660
				} else {
					if err := os.Remove(f.output); err != nil {
						t.Fatal(err)
					}
					control := filepath.Join(f.root, "mode-control")
					if err := os.WriteFile(control, nil, 0666); err != nil {
						t.Fatal(err)
					}
					st, _ := os.Stat(control)
					want = st.Mode().Perm()
					if mode == "umask-0077" {
						want = 0600
					}
				}
				_, stderr, code := f.run(t, mode, false, "--no-progress", "--tmpdir", f.scratch, "--config", f.config, "--output", f.output, f.source)
				if code != 0 {
					t.Fatalf("export %d: %s", code, stderr)
				}
				st, err := os.Stat(f.output)
				if err != nil || st.Mode().Perm() != want {
					t.Fatalf("mode got %v err %v want %o", st, err, want)
				}
				assertExportScratch(t, f.scratch, nil)
			})
		}
	}
}

func TestExportRejectsSpecialOutputBeforePreparation(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("fifo", link); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{dir, fifo, link} {
		before, err := os.Lstat(output)
		if err != nil {
			t.Fatal(err)
		}
		err = writePreparedExport(context.Background(), output, true, dir, nil, true, func([]string) (*flatten.PreparedImage, error) {
			t.Fatal("special output reached preparation/upload")
			return nil, nil
		}, exportIO{})
		if err == nil {
			t.Fatal("accepted special output", output)
		}
		after, e := os.Lstat(output)
		if e != nil || !os.SameFile(before, after) {
			t.Fatal("special output replaced", e)
		}
	}
	if _, _, err := outputPermissions(os.DevNull); err == nil {
		t.Fatal("device accepted")
	}
}

func TestExportRechecksDestinationBeforeCommit(t *testing.T) {
	f := newExportFixture(t)
	output := filepath.Join(f.root, "result.img")
	writeExportFile(t, output, []byte("previous"))
	manifestConfig := filepath.Join(f.root, "empty-manifest.yaml")
	writeExportFile(t, manifestConfig, []byte("{}\n"))
	_, stderr, code := f.run(t, "upload-destination-fifo", false, "--no-progress", "--tmpdir", f.scratch, "--config", f.config, "--output", output, "--upload", "--manifest-config", manifestConfig, f.source)
	if code != 1 || !strings.Contains(stderr, "commit artifact: destination is not a regular file") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	st, err := os.Lstat(output)
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("replaced late FIFO: %v %v", st, err)
	}
	assertExportScratch(t, f.scratch, nil)
	pending, err := filepath.Glob(filepath.Join(f.root, ".flatten-artifact-*"))
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending artifact remains: %v %v", pending, err)
	}
}

func TestExportPreservesExistingOutputOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise replacement of a foreign-owned output")
	}
	for _, upload := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload-%t", upload), func(t *testing.T) {
			f := newExportFixture(t)
			if err := os.Chown(f.output, 12345, 23456); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.output, 0640); err != nil {
				t.Fatal(err)
			}
			args := []string{"--no-progress", "--tmpdir", f.scratch, "--config", f.config, "--output", f.output}
			mode := ""
			if upload {
				manifestConfig := filepath.Join(f.root, "empty-manifest.yaml")
				writeExportFile(t, manifestConfig, []byte("{}\n"))
				args = append(args, "--upload", "--manifest-config", manifestConfig)
				mode = "upload-success"
			}
			_, stderr, code := f.run(t, mode, false, append(args, f.source)...)
			if code != 0 {
				t.Fatalf("export %d: %s", code, stderr)
			}
			info, err := os.Stat(f.output)
			if err != nil {
				t.Fatal(err)
			}
			owner := info.Sys().(*syscall.Stat_t)
			if owner.Uid != 12345 || owner.Gid != 23456 || info.Mode().Perm() != 0640 {
				t.Fatalf("replaced output uid/gid/mode: %d/%d/%o", owner.Uid, owner.Gid, info.Mode().Perm())
			}
			assertExportFile(t, f.output, expectedExportArtifact(t, f))
			assertExportScratch(t, f.scratch, nil)
		})
	}
}

func TestExportBlockedStdoutSignalEscalation(t *testing.T) {
	for _, upload := range []bool{false, true} {
		for _, escalate := range []bool{false, true} {
			t.Run(fmt.Sprintf("upload-%t/escalate-%t", upload, escalate), func(t *testing.T) {
				f := newExportFixture(t)
				// Distinct 4 KiB chunks keep this artifact larger than the pipe capacity.
				payload := make([]byte, 1<<20)
				for i := 0; i < len(payload); i += 4096 {
					copy(payload[i:], fmt.Sprintf("%064d", i))
				}
				writeExportFile(t, filepath.Join(f.source, "large"), payload)
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"-test.run=^TestExportProcess$", "--", "--export-helper", "", f.root,
					"--no-progress", "--tmpdir", f.scratch, "--config", f.config, "--output", "-"}
				if upload {
					manifestConfig := filepath.Join(f.root, "empty-manifest.yaml")
					writeExportFile(t, manifestConfig, []byte("{}\n"))
					args = append(args, "--upload", "--manifest-config", manifestConfig)
				}
				cmd := exec.Command(exe, append(args, f.source)...)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.scratch, "MKFS_EROFS_PATH=" + f.mkfs, "GOMAXPROCS=2"}
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				defer writer.Close()
				capacity, err := unix.FcntlInt(writer.Fd(), unix.F_SETPIPE_SZ, 4096)
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = writer, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				writer.Close()
				finished := make(chan error, 1)
				go func() { finished <- cmd.Wait() }()
				waited := false
				defer func() {
					if !waited {
						cmd.Process.Kill()
						<-finished
					}
				}()
				deadline := time.Now().Add(10 * time.Second)
				for {
					available, err := unix.IoctlGetInt(int(reader.Fd()), unix.TIOCINQ)
					if err != nil {
						t.Fatal(err)
					}
					if available > 0 && available <= capacity {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("export did not reach the stdout pipe")
					}
					time.Sleep(10 * time.Millisecond)
				}
				// Let the producer enter its next write with the pipe still unread.
				// A blocked atomic write need not fill every remaining pipe byte.
				time.Sleep(50 * time.Millisecond)
				if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
				// The first signal cancels the context; it cannot finish a blocked write.
				select {
				case err := <-finished:
					waited = true
					t.Fatalf("first signal bypassed blocked-write cancellation: %v: %s", err, &stderr)
				case <-time.After(100 * time.Millisecond):
				}
				if escalate {
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				} else {
					// A receiver that resumes allows ordinary cancellation and owned cleanup.
					if _, err := io.Copy(io.Discard, reader); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-finished:
					waited = true
					if err == nil {
						t.Fatal("cancelled export succeeded")
					}
				case <-time.After(5 * time.Second):
					cmd.Process.Kill()
					<-finished
					waited = true
					t.Fatal("export swallowed signal escalation")
				}
				status := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if escalate {
					if !status.Signaled() || status.Signal() != syscall.SIGTERM {
						t.Fatalf("exit %v: %s", status, &stderr)
					}
				} else {
					if status.Signaled() || !strings.Contains(stderr.String(), "context canceled") {
						t.Fatalf("exit %v: %s", status, &stderr)
					}
					assertExportScratch(t, f.scratch, nil)
				}
			})
		}
	}
}

type replaceArtifactOnClose struct {
	io.WriteCloser
	replace func()
}

func (w replaceArtifactOnClose) Close() error {
	err := w.WriteCloser.Close()
	w.replace()
	return err
}

func TestExportRejectsReplacedPendingArtifact(t *testing.T) {
	for _, stage := range []string{"prepare", "writer-close", "upload"} {
		for _, kind := range []string{"symlink", "regular"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				f := newExportFixture(t)
				sentinel := filepath.Join(f.root, "sentinel")
				var pending string
				replace := func() {
					t.Helper()
					if err := os.Remove(pending); err != nil {
						t.Fatal(err)
					}
					if kind == "symlink" {
						if err := os.Symlink(sentinel, pending); err != nil {
							t.Fatal(err)
						}
					} else {
						writeExportFile(t, pending, []byte("substituted artifact"))
					}
				}
				uploads := 0
				ops := exportIO{
					createArtifact: func(file *os.File) (io.WriteCloser, error) {
						w, err := artifactWriter(file)
						if err != nil {
							return nil, err
						}
						if stage == "writer-close" {
							return replaceArtifactOnClose{w, replace}, nil
						}
						return w, nil
					},
					ingestArtifact: func(_ context.Context, file *os.File, _ *manifest.Config, _ bool) (string, error) {
						uploads++
						if stage != "upload" {
							t.Fatal("replaced artifact reached uploader")
						}
						replace()
						info, err := file.Stat()
						if err != nil {
							return "", err
						}
						got, err := io.ReadAll(io.NewSectionReader(file, 0, info.Size()))
						if err != nil || !bytes.Equal(got, expectedExportArtifact(t, f)) {
							t.Fatalf("upload was not bound to reserved artifact: %v", err)
						}
						return strings.Repeat("a", 64), nil
					},
				}
				err := writePreparedExport(context.Background(), f.output, true, f.scratch, nil, true,
					func(excluded []string) (*flatten.PreparedImage, error) {
						pending = filepath.Join(excluded[0], "image")
						info, err := os.Stat(excluded[0])
						if err != nil || info.Mode().Perm() != 0700 {
							t.Fatalf("staging privacy: %v %v", info, err)
						}
						if stage == "prepare" {
							replace()
						}
						return flatten.PrepareDir(context.Background(), f.source, flatten.Options{TmpDir: f.scratch}, flatten.DirOptions{OutputPaths: excluded})
					}, ops)
				if err == nil || !strings.Contains(err.Error(), "temporary object was replaced") {
					t.Fatalf("replacement accepted: %v", err)
				}
				if stage == "upload" && uploads != 1 {
					t.Fatalf("uploads=%d", uploads)
				}
				assertExportFile(t, sentinel, f.retained[sentinel])
				assertExportFile(t, f.output, []byte("previous user artifact\n"))
				assertExportScratch(t, f.scratch, nil)
				pendingDirs, err := filepath.Glob(filepath.Join(f.root, ".flatten-artifact-*"))
				if err != nil || len(pendingDirs) != 0 {
					t.Fatalf("pending artifacts remain: %v %v", pendingDirs, err)
				}
			})
		}
	}
}

func TestPendingArtifactRejectsReplacedDirectory(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "output")
	writeExportFile(t, output, []byte("previous"))
	p, err := reserveArtifact(output, "")
	if err != nil {
		t.Fatal(err)
	}
	original := p.path + "-moved"
	if err := os.Rename(p.path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.path, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(p.path, "image")
	writeExportFile(t, sentinel, []byte("replacement directory"))
	if err := p.commit(context.Background()); err == nil {
		t.Fatal("replaced directory committed")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	assertExportFile(t, output, []byte("previous"))
	assertExportFile(t, sentinel, []byte("replacement directory"))
	entries, err := os.ReadDir(original)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned payload remains: %v %v", entries, err)
	}
}

func TestUploadCompletionCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ordinary UID required for actual cleanup permission failure")
	}
	for _, failure := range []string{"", "ingest", "key"} {
		t.Run(failure, func(t *testing.T) {
			f := newExportFixture(t)
			pendingRoot := t.TempDir()
			stdout, err := os.CreateTemp(t.TempDir(), "stdout-")
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
			if err != nil {
				t.Fatal(err)
			}
			oldOut, oldErr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = stdout, stderr
			defer func() {
				os.Stdout, os.Stderr = oldOut, oldErr
				stdout.Close()
				stderr.Close()
				os.Chmod(pendingRoot, 0700)
			}()
			if failure == "key" {
				if err := stdout.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ingestErr := errors.New("fixture ingest failure")
			ingested := false
			err = writePreparedExport(context.Background(), "", true, pendingRoot, nil, true,
				func(excluded []string) (*flatten.PreparedImage, error) {
					return flatten.PrepareDir(context.Background(), f.source, flatten.Options{TmpDir: f.scratch}, flatten.DirOptions{OutputPaths: excluded})
				}, exportIO{createArtifact: artifactWriter, ingestArtifact: func(_ context.Context, file *os.File, _ *manifest.Config, _ bool) (string, error) {
					info, e := file.Stat()
					if e != nil {
						return "", e
					}
					got, e := io.ReadAll(io.NewSectionReader(file, 0, info.Size()))
					if e != nil {
						return "", e
					}
					if !bytes.Equal(got, expectedExportArtifact(t, f)) {
						t.Fatal("uploader did not receive complete artifact")
					}
					ingested = true
					if e := os.Chmod(pendingRoot, 0555); e != nil {
						return "", e
					}
					if failure == "ingest" {
						return "", ingestErr
					}
					return "accepted-key-fixture", nil
				}})
			os.Stdout, os.Stderr = oldOut, oldErr
			stdout.Close()
			stderr.Close()
			key, e := os.ReadFile(stdout.Name())
			if e != nil {
				t.Fatal(e)
			}
			warning, e := os.ReadFile(stderr.Name())
			if e != nil {
				t.Fatal(e)
			}
			if !ingested {
				t.Fatal("ingest callback was not reached")
			}
			if failure == "" {
				if err != nil || string(key) != "accepted-key-fixture\n" || !strings.Contains(string(warning), "artifact committed; staging cleanup") {
					t.Fatalf("completed upload: key=%q error=%v warning=%q", key, err, warning)
				}
			} else {
				if !errors.Is(err, unix.EACCES) || len(key) != 0 || len(warning) != 0 {
					t.Fatalf("incomplete upload: key=%q error=%v warning=%q", key, err, warning)
				}
				if failure == "ingest" && !errors.Is(err, ingestErr) {
					t.Fatalf("lost primary error: %v", err)
				}
				if failure == "key" && !strings.Contains(err.Error(), "write manifest key") {
					t.Fatalf("lost key failure: %v", err)
				}
			}
			if err := os.Chmod(pendingRoot, 0700); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(pendingRoot)
			if err != nil || len(entries) != 1 {
				t.Fatalf("expected retained empty stage: %v %v", entries, err)
			}
			if err := os.Remove(filepath.Join(pendingRoot, entries[0].Name())); err != nil {
				t.Fatal(err)
			}
			assertExportScratch(t, f.scratch, nil)
		})
	}
}

func TestExportRedirectedDirectoryStreams(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr", "both"} {
		for _, location := range []string{"inside", "hardlink", "renamed"} {
			t.Run(stream+"/"+location, func(t *testing.T) {
				f := newExportFixture(t)
				want := expectedExportArtifact(t, f)
				sourceAlias := filepath.Join(f.root, "source-alias")
				if err := os.Symlink(f.source, sourceAlias); err != nil {
					t.Fatal(err)
				}
				redirected := func(name string) *os.File {
					t.Helper()
					path := filepath.Join(f.source, name)
					if location != "inside" {
						path = filepath.Join(f.root, name)
					}
					file, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { file.Close() })
					if location == "hardlink" {
						for _, alias := range []string{name, name + "-alias"} {
							if err := os.Link(path, filepath.Join(f.source, alias)); err != nil {
								t.Fatal(err)
							}
						}
					}
					if location == "renamed" {
						if err := os.Rename(path, filepath.Join(f.source, name)); err != nil {
							t.Fatal(err)
						}
					}
					return file
				}
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				output := f.output
				var artifact, diagnostics *os.File
				if stream == "stdout" || stream == "both" {
					artifact = redirected("artifact.img")
					output = "-"
				}
				if stream == "stderr" || stream == "both" {
					diagnostics = redirected("export.log")
				}
				cmd := exec.Command(exe, "-test.run=^TestExportProcess$", "--", "--export-helper", "", f.root,
					"--tmpdir", f.scratch, "--config", f.config, "--output", output, sourceAlias)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.scratch, "MKFS_EROFS_PATH=" + f.mkfs, "GOMAXPROCS=2"}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if artifact != nil {
					cmd.Stdout = artifact
				}
				if diagnostics != nil {
					cmd.Stderr = diagnostics
				}
				err = cmd.Run()
				if diagnostics != nil {
					if _, seekErr := diagnostics.Seek(0, io.SeekStart); seekErr != nil {
						t.Fatal(seekErr)
					}
					body, readErr := io.ReadAll(diagnostics)
					if readErr != nil {
						t.Fatal(readErr)
					}
					stderr.Write(body)
				}
				if err != nil {
					t.Fatalf("export: %v: %s", err, stderr.String())
				}
				if !strings.Contains(stderr.String(), "flatten: build-erofs") {
					t.Fatal("progress did not exercise redirected diagnostics")
				}
				if artifact != nil {
					if _, err := artifact.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(artifact)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("redirected artifact differs: got %d want %d", len(got), len(want))
					}
				} else {
					assertExportFile(t, output, want)
				}
				assertExportScratch(t, f.scratch, nil)
			})
		}
	}
}

// A real planner failure after reserving the destination must preserve both
// the previous output and unrelated files in shared scratch.
func TestExportPlanBudgetFailure(t *testing.T) {
	f := newExportFixture(t)
	sentinel := []byte("unrelated scratch\n")
	writeExportFile(t, filepath.Join(f.scratch, "flatten-prepared-user"), sentinel)
	err := writePreparedExport(context.Background(), f.output, false, f.scratch, nil, true,
		func(excluded []string) (*flatten.PreparedImage, error) {
			return flatten.PrepareDir(context.Background(), f.source,
				flatten.Options{TmpDir: f.scratch, PlanLimits: flatten.PlanLimits{PlanBytes: 192}},
				flatten.DirOptions{OutputPaths: excluded})
		}, exportIO{})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected plan budget failure, got %v", err)
	}
	assertExportFile(t, f.output, []byte("previous user artifact\n"))
	for path, data := range f.retained {
		assertExportFile(t, path, data)
	}
	assertExportScratch(t, f.scratch, map[string][]byte{"flatten-prepared-user": sentinel})
	if err := checkExportRawClosed(f.scratch); err != nil {
		t.Fatal(err)
	}
}
