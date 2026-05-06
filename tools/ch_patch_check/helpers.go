package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	randSeed    = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))
	stderrSink  = os.Stderr
)

// monitorReader streams stdout/stderr of CH and signals when a marker
// line shows up (or when EOF / timeout). It also prints to stderr live
// so the operator can see what's happening.
type monitorReader struct {
	prefix string
	marker string
	hit    chan struct{}
	once   chan struct{}
	pw     io.WriteCloser
	pr     *io.PipeReader
}

func newMonitorReader(prefix, marker string) *monitorReader {
	pr, pw := io.Pipe()
	m := &monitorReader{
		prefix: prefix,
		marker: marker,
		hit:    make(chan struct{}),
		once:   make(chan struct{}),
		pw:     pw,
		pr:     pr,
	}
	go m.scan()
	return m
}

func (m *monitorReader) scan() {
	buf := make([]byte, 4096)
	var line strings.Builder
	signaled := false
	for {
		n, err := m.pr.Read(buf)
		for i := 0; i < n; i++ {
			c := buf[i]
			if c == '\n' {
				s := line.String()
				fmt.Fprintf(stderrSink, "[%s] %s\n", m.prefix, s)
				if !signaled && m.marker != "" && strings.Contains(s, m.marker) {
					signaled = true
					close(m.hit)
				}
				line.Reset()
			} else {
				line.WriteByte(c)
			}
		}
		if err != nil {
			return
		}
	}
}

// pickRunDir returns a short tmp dir under /tmp suitable for hosting
// AF_UNIX sockets (SUN_LEN max is ~108 bytes; our repo's absolute path
// alone exceeds that comfortably).
func pickRunDir(prefix string) (string, error) {
	dir := filepath.Join("/tmp", fmt.Sprintf("ch-pc-%s-%d", prefix, randSeed.Uint32()))
	if err := mkdirP(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func mkdirP(p string) error {
	cmd := exec.Command("mkdir", "-p", p)
	return cmd.Run()
}

// startCH spawns cloud-hypervisor with the given args + ExtraFiles. It
// pipes stdout+stderr through `mon`. Caller owns the *exec.Cmd and is
// responsible for Wait/Kill.
func startCH(
	ctx context.Context,
	cf commonFlags,
	mon *monitorReader,
	args []string,
	extraFiles []*os.File,
) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, cf.chBin, args...)
	cmd.Stdout = mon.pw
	cmd.Stderr = mon.pw
	cmd.ExtraFiles = extraFiles
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
