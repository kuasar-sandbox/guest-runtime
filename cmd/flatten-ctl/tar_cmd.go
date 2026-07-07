package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	btar "github.com/kuasar-sandbox/accelerator/pkg/tar"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// cmdTar implements the tar tooling, all pure Go:
//
//	flatten-ctl tar extract — pull files out of any tar stream
//	  (single-pass streaming; a single-file rule restores declared
//	  holes exactly)
//	flatten-ctl tar stream  — package one file or sized stdin as a
//	  tarstream (accelerator pkg/tarstream)
func cmdTar(args []string) {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" {
		tarUsage()
		if len(args) < 1 {
			os.Exit(1)
		}
		return
	}
	sub := args[0]
	switch sub {
	case "extract":
		cmdTarExtract(args[1:])
	case "stream":
		cmdTarStream(args[1:])
	default:
		fatal("tar: unknown subcommand %q (want extract or stream)", sub)
	}
}

func cmdTarExtract(args []string) {
	fs := flag.NewFlagSet("tar extract", flag.ExitOnError)
	var file string
	fs.StringVar(&file, "file", "-", "archive file (- = stdin)")
	fs.StringVar(&file, "f", "-", "shorthand for --file")
	chown := fs.String("chown", "", "uid:gid applied to every entry (numeric, or names resolved against the target's /etc/passwd|group)")
	chmod := fs.String("chmod", "", "octal permission bits applied to every entry")
	noChown := fs.Bool("no-chown", false, "skip ownership restoration")
	dense := fs.Bool("dense", false, "disable sparse handling: extract logical bytes via the stdlib reader (declared holes become allocated zeros)")
	fs.Usage = tarUsage
	fs.Parse(args)

	rules, err := btar.ParseRules(fs.Args())
	if err != nil {
		fatal("tar: %v", err)
	}
	opts := btar.Options{
		Dense:   *dense,
		NoChown: *noChown,
		Warnf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		},
	}
	if *chown != "" {
		o, err := btar.ParseOwner(*chown)
		if err != nil {
			fatal("tar: %v", err)
		}
		opts.Chown = &o
	}
	if *chmod != "" {
		m, err := btar.ParseMode(*chmod)
		if err != nil {
			fatal("tar: %v", err)
		}
		opts.Chmod = &m
	}
	if file != "-" {
		// A re-openable archive lets the engine extract sparse members
		// hole-exact (re-located by ordinal on a second handle).
		opts.Reopen = func() (io.ReadSeekCloser, error) { return os.Open(file) }
	}

	openArchive := func() io.Reader {
		if file == "-" {
			return os.Stdin
		}
		f, err := os.Open(file)
		if err != nil {
			fatal("tar extract: %v", err)
		}
		return f // process exit closes it
	}

	// No rules + a single-entry archive file (the platform artifact
	// shape): unwrap the one entry hole-exact — `tar extract -f x.img`
	// just works. Multi-entry archives fall through to the generic
	// engine (which is also hole-exact via Reopen).
	if len(fs.Args()) == 0 && !*dense && file != "-" && singleEntryArtifact(file) {
		f, err := os.Open(file)
		if err != nil {
			fatal("tar extract: %v", err)
		}
		defer f.Close()
		v, err := tarstream.ReadFrom(f, "")
		if err == nil {
			if err := btar.ExtractFile(v, filepath.FromSlash(v.Name()), opts); err != nil {
				fatal("%v", err)
			}
			return
		}
		// e.g. the single entry is not a regular file: generic engine.
	}

	in := openArchive()

	// Single explicit file rule: route through the tarstream view so a
	// sparse member's DECLARED holes are restored exactly even from
	// stdin (single-pass). A rule that turns out to name a directory
	// falls back to the generic engine — possible only when the
	// archive can be reopened.
	if member, dst, ok := singleFileRule(fs.Args()); ok && !*dense {
		v, err := tarstream.ReadFrom(in, member)
		if err == nil {
			if err := btar.ExtractFile(v, dst, opts); err != nil {
				fatal("%v", err)
			}
			return
		}
		if file == "-" {
			fatal("tar extract: %q: %v (stdin is single-pass: use -f FILE for directory members or legacy sparse encodings, or --dense)", member, err)
		}
		in = openArchive() // rewind by reopening; the generic engine re-reads and reports authoritatively
	}
	if err := btar.Extract(in, rules, opts); err != nil {
		fatal("%v", err)
	}
}

// singleEntryArtifact reports whether the archive at path holds exactly
// one real entry (the platform artifact shape).
func singleEntryArtifact(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = tarstream.ReadSeekFromIndex(f, 1)
	return errors.Is(err, tarstream.ErrNotFound)
}

// singleFileRule reports whether the raw extract arguments are exactly
// one explicit file rule "member[:dst]" (dst a path — not stdout, not
// a directory form, not the archive root).
func singleFileRule(args []string) (member, dst string, ok bool) {
	if len(args) != 1 {
		return "", "", false
	}
	arg := args[0]
	member, dst, found := strings.Cut(arg, ":")
	if !found {
		dst = member
	}
	if member == "" || strings.HasSuffix(member, "/") { // archive root / directory rule
		return "", "", false
	}
	if dst == "" || dst == "-" || strings.HasSuffix(dst, "/") {
		return "", "", false
	}
	return member, dst, true
}

func cmdTarStream(args []string) {
	fs := flag.NewFlagSet("tar stream", flag.ExitOnError)
	var file string
	fs.StringVar(&file, "file", "-", "tarstream output (- = stdout)")
	fs.StringVar(&file, "f", "-", "shorthand for --file")
	size := fs.Int64("size", -1, "stdin byte count (required for a stdin source; rejected for file sources)")
	fs.Usage = tarUsage
	fs.Parse(args)

	if fs.NArg() > 1 {
		fatal("tar stream: packages exactly one file (got %d rules)", fs.NArg())
	}
	name, srcArg, err := parseStreamRule(fs.Arg(0))
	if err != nil {
		fatal("tar stream: %v", err)
	}

	var out io.Writer
	if file == "-" {
		if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			fatal("tar stream: refusing to write a tar stream to a terminal (use -f FILE or redirect stdout)")
		}
		out = os.Stdout
	} else {
		f, err := os.Create(file)
		if err != nil {
			fatal("tar stream: %v", err)
		}
		defer f.Close()
		out = f
	}

	// Resolve the source as a sparse.Source. A file's hole map comes
	// from filesystem metadata (SEEK_HOLE); stdin is dense by
	// definition (no authoritative hole metadata — content is never
	// scanned for holes) and streams straight through: the tar header
	// needs the size up front, so --size is required and nothing is
	// spooled.
	var src sparse.Source
	if srcArg == "-" {
		if *size < 0 {
			fatal("tar stream: a stdin source needs --size N (the tar header carries the size up front; nothing is spooled)")
		}
		src = sparse.Dense(os.Stdin, uint64(*size))
	} else {
		if *size >= 0 {
			fatal("tar stream: --size is for stdin sources only (file sizes come from the filesystem)")
		}
		f, err := os.Open(srcArg)
		if err != nil {
			fatal("tar stream: %v", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			fatal("tar stream: %v", err)
		}
		if !st.Mode().IsRegular() {
			fatal("tar stream: %s is not a regular file (pipe it in with - and --size)", srcArg)
		}
		holes, err := sparse.ProbeHoles(f)
		if err != nil {
			fatal("tar stream: probe holes: %v", err)
		}
		src, err = sparse.NewSource(f, uint64(st.Size()), holes)
		if err != nil {
			fatal("tar stream: %v", err)
		}
	}

	if err := tarstream.WriteTo(context.Background(), out, name, src); err != nil {
		fatal("%v", err)
	}
}

// parseStreamRule normalizes the stream argument "in-tar[:source]":
//
//	(none)        ≡ -:-
//	-             ≡ -:-
//	path/to/file  ≡ file:path/to/file   (named after its base)
//	:source       ≡ -:source
//	name:-        content from stdin
//	name:source   explicit
func parseStreamRule(arg string) (name, src string, err error) {
	switch arg {
	case "", "-":
		return "-", "-", nil
	}
	if i := strings.Index(arg, ":"); i >= 0 {
		name, src = arg[:i], arg[i+1:]
		if name == "" {
			name = "-"
		}
		if src == "" {
			return "", "", fmt.Errorf("rule %q: missing source (use %s:- for stdin)", arg, name)
		}
		return name, src, nil
	}
	return path.Base(arg), arg, nil
}

func tarUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  flatten-ctl tar extract [-f tarfile] [--chown u:g] [--no-chown] [--chmod 755] [rule...]
  flatten-ctl tar stream  [-f tarfile] [--size N] [in-tar[:source]]

Both directions are pure Go; no tar binary is involved.

extract pulls files out of any tar stream (-f defaults to stdin), one
single pass — pipes need no spooling. Regular files are written dense:
zero bytes are data and stay allocated, never turned into holes. A
single explicit file rule restores the member's DECLARED holes exactly
(sparse map fidelity); directory extraction materializes sparse
members dense. Rules are "in-tar-path[:outside-path]":

  path              same name inside and outside
  in:out            rename (write the entry "in" to the path "out")
  in:-              stream the entry's content to stdout (one rule at most)
  dir/              directory rule: dir and everything under it
  dir/:out[/]       directory prefix rename
  dir/:              extract under the current directory
  :dir/             the whole archive root mapped to dir/

No rules takes everything. --chown/--chmod override ownership and
permissions on every extracted entry. --no-chown skips ownership
restoration when uid/gid metadata is irrelevant to the extraction.

stream packages exactly one file as a tarstream (a single-file sparse
tar; see accelerator/pkg/tarstream). A file source's holes
come from the filesystem (SEEK_HOLE) — never from scanning content. A
stdin source requires --size N (the tar header carries the size up
front), streams straight through with nothing spooled, and is packaged
dense. The argument is "in-tar[:source]":

  (none) or -       ≡ -:-  (entry named "-", content from stdin)
  path/to/file      ≡ file:path/to/file  (named after its base)
  :source           ≡ -:source
  name:-            entry "name", content from stdin
  name:source       explicit

The output (-f, default stdout) is itself a valid tar: extract, GNU tar
and archive/tar all read it back.
`)
}
