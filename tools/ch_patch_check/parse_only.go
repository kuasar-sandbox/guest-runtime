package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os/exec"
	"strings"
)

// runParseOnly probes commits the memory-zone fd= parser path: the parser must accept fd= /
// uffd_fd= / uffd_socket= without rejecting them as unknown keys.
//
// We don't need a working VM here. We just need the parser to get
// past those keys and reach the next stage. Two cases prove it:
//
//   (1) All three keys with bogus values → the parser doesn't reject
//       them; the failure shows up later (boot-time fd usage).
//   (2) uffd_fd alone → the explicit pairing-validation we added in
//       the parser-error path fires with a known message.
//
// Both behaviours are observed in the stderr of a CH invocation.
func runParseOnly(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("parse-only", flag.ContinueOnError)
	var cf commonFlags
	cf.bind(fs, 30)
	if err := fs.Parse(argv); err != nil {
		return err
	}

	// (1) all three keys present, bogus fds. Expected substring shows
	//     the parser accepted them and CH moved on to MemoryManager.
	out1, _ := runCH(ctx, cf,
		"--memory", "size=0",
		"--memory-zone",
		"id=z0,size=4M,fd=999,uffd_fd=998,uffd_socket=/tmp/no-such-sock-"+nonceSuffix(),
	)
	wantErr1 := []string{"Bad file descriptor"}
	if !containsAny(out1, wantErr1) {
		return fmt.Errorf("case 1: stderr lacked any of %v\nout=\n%s", wantErr1, out1)
	}
	if hasAnyParseError(out1, []string{"unknown key", "Unknown key"}) {
		return fmt.Errorf("case 1: parser rejected new keys\nout=\n%s", out1)
	}

	// (2) uffd_fd without uffd_socket → explicit pairing rejection.
	out2, _ := runCH(ctx, cf,
		"--memory", "size=0",
		"--memory-zone",
		"id=z0,size=4M,uffd_fd=998",
	)
	wantErr2 := "uffd_fd and uffd_socket must be set together"
	if !strings.Contains(out2, wantErr2) {
		return fmt.Errorf("case 2: missing pairing-validation message %q\nout=\n%s", wantErr2, out2)
	}

	return nil
}

func runCH(ctx context.Context, cf commonFlags, args ...string) (string, error) {
	all := append([]string{"--kernel", cf.vmlinux}, args...)
	cmd := exec.CommandContext(ctx, cf.chBin, all...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func hasAnyParseError(s string, markers []string) bool {
	return containsAny(s, markers)
}

// nonceSuffix returns a short non-cryptographic suffix for socket
// paths so successive runs don't conflict on shared tmp dirs.
func nonceSuffix() string {
	return fmt.Sprintf("%d", randSeed.Uint32())
}
