// Package stdio resolves sandbox-ctl run's stdio model into a
// concrete os/exec.Cmd Stdin/Stdout/Stderr wiring.
//
// The CLI flag set is:
//
//	--stdin / --stdout / --stderr  (bool; default false / true / true)
//	--stdin-from / --stdout-to / --stderr-to FILE
//	--tty                          (bool; default false)
//
// See docs/sandbox.md §2.2 for semantics + decision table + mutual
// exclusion rules.
//
// Apply() opens any required fd, sets cmd.Stdin/Stdout/Stderr, and
// returns a cleanup func the caller must invoke after cmd.Wait().
package stdio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Mode is the parsed stdio configuration for one CH spawn.
type Mode struct {
	// TTY, if true, allocates a pty pair and routes all three streams
	// to the slave end. Mutually exclusive with the per-stream fields
	// below — Validate() enforces.
	TTY bool

	// Per-stream choices. Each is exactly one of (Inherit, DevNull, File).
	Stdin  Stream
	Stdout Stream
	Stderr Stream
}

// Stream describes how one of {stdin, stdout, stderr} is wired.
type Stream struct {
	// Kind selects between inherit / /dev/null / open-file.
	Kind StreamKind
	// Path is required when Kind == StreamFile.
	Path string
}

type StreamKind int

const (
	StreamDevNull StreamKind = iota // route to /dev/null
	StreamInherit                   // inherit os.Stdin/Stdout/Stderr
	StreamFile                      // open Path with O_RDONLY (stdin) or O_WRONLY|O_CREATE|O_APPEND (stdout/stderr)
)

// Defaults is the zero-flag default: stdin=DevNull, stdout/err=Inherit.
var Defaults = Mode{
	Stdin:  Stream{Kind: StreamDevNull},
	Stdout: Stream{Kind: StreamInherit},
	Stderr: Stream{Kind: StreamInherit},
}

// FromFlags resolves CLI flags into a validated Mode. The bools and
// strings come straight from flag.FlagSet; this function applies the
// "implies enabled" / mutual-exclusion rules.
//
// Bools are tri-state via *bool: nil = unset (defaults apply), &false /
// &true = explicit assignment. The CLI layer should construct *bool by
// detecting whether the flag was provided.
func FromFlags(stdin, stdout, stderr *bool, stdinFrom, stdoutTo, stderrTo string, tty bool) (Mode, error) {
	m := Defaults

	if tty {
		if stdin != nil || stdout != nil || stderr != nil ||
			stdinFrom != "" || stdoutTo != "" || stderrTo != "" {
			return Mode{}, errors.New("--tty conflicts with explicit stdio flags")
		}
		return Mode{TTY: true}, nil
	}

	// stdin.
	if stdin != nil && !*stdin && stdinFrom != "" {
		return Mode{}, errors.New("--stdin=false conflicts with --stdin-from")
	}
	switch {
	case stdinFrom != "":
		m.Stdin = Stream{Kind: StreamFile, Path: stdinFrom}
	case stdin != nil && *stdin:
		m.Stdin = Stream{Kind: StreamInherit}
	case stdin != nil && !*stdin:
		m.Stdin = Stream{Kind: StreamDevNull}
		// nil → keep default (DevNull)
	}

	// stdout.
	if stdout != nil && !*stdout && stdoutTo != "" {
		return Mode{}, errors.New("--stdout=false conflicts with --stdout-to")
	}
	switch {
	case stdoutTo != "":
		m.Stdout = Stream{Kind: StreamFile, Path: stdoutTo}
	case stdout != nil && *stdout:
		m.Stdout = Stream{Kind: StreamInherit}
	case stdout != nil && !*stdout:
		m.Stdout = Stream{Kind: StreamDevNull}
	}

	// stderr.
	if stderr != nil && !*stderr && stderrTo != "" {
		return Mode{}, errors.New("--stderr=false conflicts with --stderr-to")
	}
	switch {
	case stderrTo != "":
		m.Stderr = Stream{Kind: StreamFile, Path: stderrTo}
	case stderr != nil && *stderr:
		m.Stderr = Stream{Kind: StreamInherit}
	case stderr != nil && !*stderr:
		m.Stderr = Stream{Kind: StreamDevNull}
	}

	return m, nil
}

// Apply binds the Mode onto cmd and returns cleanup. Cleanup must be
// invoked once cmd has exited (typically via defer). Caller may pass
// extra closers (e.g. previously allocated fds) which cleanup will close.
func (m Mode) Apply(cmd *exec.Cmd) (cleanup func(), err error) {
	if m.TTY {
		return applyTTY(cmd)
	}

	var closers []io.Closer
	cleanup = func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	stdinFD, c, err := openStdin(m.Stdin)
	if err != nil {
		return nil, err
	}
	if c != nil {
		closers = append(closers, c)
	}
	cmd.Stdin = stdinFD

	stdoutFD, c, err := openOutput(m.Stdout, os.Stdout)
	if err != nil {
		return nil, err
	}
	if c != nil {
		closers = append(closers, c)
	}
	cmd.Stdout = stdoutFD

	stderrFD, c, err := openOutput(m.Stderr, os.Stderr)
	if err != nil {
		return nil, err
	}
	if c != nil {
		closers = append(closers, c)
	}
	cmd.Stderr = stderrFD

	return cleanup, nil
}

// openStdin returns (reader, closer, err) for cmd.Stdin per the Stream.
// The returned closer is non-nil when the helper opened a new fd.
func openStdin(s Stream) (io.Reader, io.Closer, error) {
	switch s.Kind {
	case StreamInherit:
		return os.Stdin, nil, nil
	case StreamDevNull:
		f, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("stdio: open /dev/null: %w", err)
		}
		return f, f, nil
	case StreamFile:
		f, err := os.OpenFile(s.Path, os.O_RDONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("stdio: open %s: %w", s.Path, err)
		}
		return f, f, nil
	default:
		return nil, nil, fmt.Errorf("stdio: invalid stdin kind %d", s.Kind)
	}
}

// openOutput returns (writer, closer, err) for cmd.Stdout / cmd.Stderr.
// inherit is the parent process's corresponding *os.File.
func openOutput(s Stream, inherit *os.File) (io.Writer, io.Closer, error) {
	switch s.Kind {
	case StreamInherit:
		return inherit, nil, nil
	case StreamDevNull:
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("stdio: open /dev/null: %w", err)
		}
		return f, f, nil
	case StreamFile:
		f, err := os.OpenFile(s.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("stdio: open %s: %w", s.Path, err)
		}
		return f, f, nil
	default:
		return nil, nil, fmt.Errorf("stdio: invalid stream kind %d", s.Kind)
	}
}

// applyTTY allocates a pty pair, routes the slave end to all three of
// cmd's standard streams, and starts goroutines to bridge the master
// end to sandbox-ctl's own stdio.
func applyTTY(cmd *exec.Cmd) (cleanup func(), err error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = master.Close()
			_ = slave.Close()
		}
	}()

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true

	// Bridge master to sandbox-ctl's own stdio. Goroutines exit when
	// the pty closes (master close → io.Copy returns).
	doneIn := make(chan struct{})
	doneOut := make(chan struct{})
	go func() {
		defer close(doneIn)
		_, _ = io.Copy(master, os.Stdin)
	}()
	go func() {
		defer close(doneOut)
		_, _ = io.Copy(os.Stdout, master)
	}()

	cleanup = func() {
		_ = slave.Close()
		_ = master.Close()
		<-doneIn
		<-doneOut
	}
	return cleanup, nil
}

// openPTY allocates a /dev/ptmx + matching pts/N pair via the standard
// glibc-equivalent ioctl sequence: open ptmx → unlock pts → look up
// slave name → open slave.
func openPTY() (master, slave *os.File, err error) {
	mfd, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("stdio: open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = mfd.Close()
		}
	}()
	// TIOCSPTLCK 0 = unlockpt.
	if err := unix.IoctlSetPointerInt(int(mfd.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, fmt.Errorf("stdio: unlockpt: %w", err)
	}
	// TIOCGPTN gives the pts number.
	n, err := unix.IoctlGetInt(int(mfd.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, nil, fmt.Errorf("stdio: TIOCGPTN: %w", err)
	}
	slaveName := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := os.OpenFile(slaveName, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("stdio: open %s: %w", slaveName, err)
	}
	return mfd, sfd, nil
}
