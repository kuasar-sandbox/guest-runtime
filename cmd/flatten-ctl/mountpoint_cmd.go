package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdMountpoint makes a directory a mount point by bind-mounting it
// onto itself (creating it first). Purpose: a guest-side build flow
// marks its scratch/output directory this way so `flatten-ctl export
// --skip-mounts /` excludes it from the exported image — the mkfs
// temp files and the output artifact never leak into the product.
// Linux-only (the only place sandbox builds run).
func cmdMountpoint(args []string) {
	fs := flag.NewFlagSet("mountpoint", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: flatten-ctl mountpoint <dir>

Creates <dir> (and parents) and bind-mounts it onto itself, making it
a mount point. export --skip-mounts then excludes it — use it as the
tmpdir/output home when exporting the rootfs it lives in.
`)
	}
	fs.Parse(args)
	dir := fs.Arg(0)
	if dir == "" || fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if err := selfBind(dir); err != nil {
		fatal("mountpoint: %v", err)
	}
}
