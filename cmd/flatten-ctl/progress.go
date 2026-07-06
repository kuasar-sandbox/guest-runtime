package main

import (
	"fmt"
	"os"
	"time"
)

// Progress goes to stderr because stdout carries deliverables (the manifest
// key, --print-digest output, or the EROFS itself under --output -). Every
// helper here is a no-op / nil when --no-progress is set.

// progressf writes one progress line to stderr when enabled.
func progressf(enabled bool, format string, a ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// byteProgress builds a throttled (processed, total) callback for the manifest
// ingester's OnProgress hook, rendering a percent + rate line at most once
// every 2s (and always on the final, processed==total call). Modelled on
// sandbox-ctl's snapshot upload progress. Returns nil when disabled so callers
// pass no callback at all. Both arguments are byte counts.
func byteProgress(enabled bool, label string) func(processed, total uint64) {
	if !enabled {
		return nil
	}
	const mib = 1 << 20
	start := time.Now()
	last := start
	var lastProcessed uint64
	return func(processed, total uint64) {
		now := time.Now()
		if processed < total && now.Sub(last) < 2*time.Second {
			return
		}
		rate := 0.0
		if elapsed := now.Sub(last).Seconds(); elapsed > 0 {
			rate = float64(processed-lastProcessed) / elapsed / mib
		}
		pct := uint64(0)
		if total > 0 {
			pct = processed * 100 / total
			if pct > 100 {
				pct = 100
			}
		}
		fmt.Fprintf(os.Stderr, "upload: %s %d/%d MiB (%d%%) %.0f MiB/s\n",
			label, processed/mib, total/mib, pct, rate)
		last, lastProcessed = now, processed
	}
}

// flattenProgress builds a flatten.Options.Progress callback that renders each
// Build stage to stderr, or nil when disabled. The "extract-archive" stage
// carries byte counts and fires per Read, so it is throttled to one line every
// 2s (plus a final line) and rendered in MiB; the discrete stages print as they
// arrive.
func flattenProgress(enabled bool) func(stage string, done, total int) {
	if !enabled {
		return nil
	}
	const mib = 1 << 20
	var lastExtract time.Time
	return func(stage string, done, total int) {
		if stage == "extract-archive" {
			now := time.Now()
			final := total > 0 && done >= total
			if !final && now.Sub(lastExtract) < 2*time.Second {
				return
			}
			lastExtract = now
			if total > 0 {
				pct := done * 100 / total
				if pct > 100 {
					pct = 100
				}
				fmt.Fprintf(os.Stderr, "flatten: extract-archive %d/%d MiB (%d%%)\n", done/mib, total/mib, pct)
			} else {
				fmt.Fprintf(os.Stderr, "flatten: extract-archive %d MiB\n", done/mib)
			}
			return
		}
		if total > 0 {
			fmt.Fprintf(os.Stderr, "flatten: %s %d/%d\n", stage, done, total)
		} else {
			fmt.Fprintf(os.Stderr, "flatten: %s\n", stage)
		}
	}
}

// pullProgress builds a remote.Config.OnPullProgress callback, or nil when
// disabled. Layer downloads run concurrently, so lines may interleave order.
func pullProgress(enabled bool) func(done, total int) {
	if !enabled {
		return nil
	}
	return func(done, total int) {
		fmt.Fprintf(os.Stderr, "pull: %d/%d layers\n", done, total)
	}
}
