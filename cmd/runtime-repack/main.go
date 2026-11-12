// Command runtime-repack replaces files inside a sandbox-runtime.bundle and
// republishes the bundle with a fresh digest identity.
//
// The bundle layout is raw EROFS + 2 MiB alignment padding + a trailing
// digest-marker ZIP (internal/runtimebundle). This command streams the source
// image through the pure-Go EROFS reader, overlays the requested payload
// files (typically a locally rebuilt sandbox-init), writes a new EROFS with
// the release-layout settings — 4096-byte blocks, zero build time, no inline
// data, no xattrs — wraps it with the canonical footer, and verifies
// byte-for-byte that every non-payload entry survived the round trip
// unchanged. Payload entries are published with mode 0777, root ownership,
// and zero mtime.
//
// Usage — two subcommands; the common case is replace with the bundle
// followed by payload files:
//
//	runtime-repack replace old.bundle ./sandbox-init
//
// The first positional is the source bundle; every following positional is a
// payload file. Payload basenames select the target: "init" or
// "sandbox-init" replace /sbin/init; any other name replaces (or, if the
// image lacks it, adds) the same-named file under /opt/sandbox-runtime/bin/.
// The rebuilt bundle is written to the working directory under the source
// basename; writing over the source itself keeps the original as
// <bundle>.bak-<old digest prefix> keyed by identity.
//
// The other subcommand verifies and reports:
//
//	runtime-repack inspect old.bundle
//
// Full control stays available on replace (one or two dashes; --x is the
// documented spelling):
//
//	runtime-repack replace --bundle old.bundle --output new.bundle --replace /sbin/init=./sandbox-init
//
// Repeated -replace flags are allowed; each is IMAGE-PATH=LOCAL-FILE, split
// at the first '='. The image path must already exist as a regular file;
// replacing directories, symlinks, or hard-linked entries is rejected. Local
// paths containing '=' are not supported.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	erofs "github.com/kuasar-sandbox/go-erofs"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/guest-runtime/internal/runtimebundle"
)

const pmemAlignment = int64(2 << 20)

// erofsSuperOffset / field offsets mirror accelerator/pkg/image; only the
// fields needed to bound the raw EROFS prefix are read here.
const (
	erofsSuperOffset = 1024
	erofsMagic       = 0xE0F5E1E2
	erofsOffBlkBits  = 12
	erofsOffBlocks   = 36
)

const usage = `runtime-repack replaces or adds files inside a sandbox runtime bundle
(EROFS image + digest footer) and repacks it — no mkfs.erofs, no root.

usage:
  runtime-repack inspect BUNDLE
  runtime-repack replace BUNDLE [PAYLOAD...]

examples, simple to complex:

  # 1. Inspect a bundle: verify the digest and list its entries.
  runtime-repack inspect sandbox-runtime.bundle

  # 2. Replace /sbin/init. The payload must be named init or sandbox-init.
  #     The rebuilt bundle lands in the working directory, under the
  #     source's basename.
  runtime-repack replace sandbox-runtime.bundle ./sandbox-init

  # 3. Update (or add) files under /opt/sandbox-runtime/bin/ — the payload
  #     basename is the target file name.
  runtime-repack replace sandbox-runtime.bundle ./envd ./flatten-ctl

  # 4. Both at once.
  runtime-repack replace sandbox-runtime.bundle ./sandbox-init ./envd

  # 5. In-place: run inside the bundle's directory, so the output path
  #     equals the source; the original is kept as
  #     sandbox-runtime.bundle.bak-<old digest prefix>.
  cd /opt/sandbox/nova/bin && runtime-repack replace sandbox-runtime.bundle ./sandbox-init

  # 6. Fully explicit: arbitrary image paths and a chosen output path.
  runtime-repack replace --bundle old.bundle \
      --replace /sbin/init=./sandbox-init \
      --replace /opt/sandbox-runtime/bin/envd=./envd \
      --output new.bundle

  # 7. Verify a swap end to end: inspect reports the identity digest and
  #     per-file hashes — run it before, then after the replace.
  runtime-repack inspect sandbox-runtime.bundle
  runtime-repack replace sandbox-runtime.bundle ./sandbox-init
  runtime-repack inspect sandbox-runtime.bundle

rules:
  - replace: payload basenames select targets — init/sandbox-init go to
    /sbin/init; any other name goes to /opt/sandbox-runtime/bin/<name>,
    replacing it or adding it if absent
  - payload entries are published mode 0777, root:root, mtime 0
  - output is deterministic: identical inputs rebuild to an identical digest
  - repacking changes the bundle identity; templates pinned to the old
    digest (memory snapshots) refuse to resume against the new bundle`

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr, nil)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "inspect":
		cmdInspect(args)
	case "replace":
		cmdReplace(args)
	case "help":
		printUsage(os.Stdout, nil)
	default:
		fmt.Fprintf(os.Stderr, "runtime-repack: unknown command %q\n\n", cmd)
		printUsage(os.Stderr, nil)
		os.Exit(2)
	}
}

// printUsage writes the usage text, plus the given subcommand's flags when
// invoked from one of its flag sets. Flags are shown GNU-style with two
// dashes; the parser accepts one or two dashes equally.
func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, usage)
	if fs != nil {
		fmt.Fprint(w, "\nflags (--x and -x are equivalent):\n")
		fs.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(w, "  --%s\n    \t%s\n", f.Name, f.Usage)
		})
	}
}

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stderr, fs) }
	fs.Parse(args)
	if n := len(fs.Args()); n != 1 {
		fmt.Fprintf(os.Stderr, "runtime-repack inspect: takes exactly one bundle argument, got %d\n", n)
		os.Exit(2)
	}
	if err := runInspect(fs.Args()[0]); err != nil {
		fmt.Fprintf(os.Stderr, "runtime-repack inspect: %v\n", err)
		os.Exit(1)
	}
}

func cmdReplace(args []string) {
	fs := flag.NewFlagSet("replace", flag.ExitOnError)
	var (
		bundleFlag = fs.String("bundle", "", "source runtime bundle path (default: first positional)")
		outputFlag = fs.String("output", "", "output runtime bundle path (default: ./<source basename>)")
	)
	var replaces replaceSpecs
	fs.Var(&replaces, "replace", "IMAGE-PATH=LOCAL-FILE, e.g. /sbin/init=./sandbox-init (repeatable; existing image paths only)")
	fs.Usage = func() { printUsage(os.Stderr, fs) }
	fs.Parse(args)
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "runtime-repack replace: "+format+"\n", args...)
		os.Exit(2)
	}

	// First positional is the source bundle; the rest are payload files.
	bundle, payloads := *bundleFlag, fs.Args()
	if bundle == "" {
		positional := fs.Args()
		if len(positional) == 0 {
			printUsage(os.Stderr, fs)
			os.Exit(2)
		}
		bundle, payloads = positional[0], positional[1:]
		if !looksLikeBundle(bundle) {
			fail("%s is not a runtime bundle (expected 2 MiB alignment and EROFS magic); pass the bundle first, payload files after", bundle)
		}
	}
	for _, p := range payloads {
		if looksLikeBundle(p) {
			fail("%s looks like a runtime bundle, not a payload; the bundle comes first", p)
		}
	}

	specs := replaces
	seen := map[string]string{}
	for _, p := range payloads {
		spec, err := payloadSpec(p)
		if err != nil {
			fail("%v", err)
		}
		if prev, dup := seen[spec.imagePath]; dup {
			fail("duplicate payload for %s (from %s and %s)", spec.imagePath, prev, p)
		}
		seen[spec.imagePath] = p
		specs = append(specs, spec)
	}
	output := *outputFlag
	if output == "" {
		output = filepath.Base(bundle) // working directory
	}
	if err := runRepack(bundle, output, specs); err != nil {
		fmt.Fprintf(os.Stderr, "runtime-repack replace: %v\n", err)
		os.Exit(1)
	}
}

// payloadSpec maps a positional payload file to its image target by
// basename: "init" and "sandbox-init" become the guest PID 1, anything else
// replaces (or, if the image lacks it, adds) the same-named file under
// /opt/sandbox-runtime/bin/.
func payloadSpec(localPath string) (replaceSpec, error) {
	base := filepath.Base(filepath.Clean(localPath))
	switch base {
	case "", ".", "..", "/":
		return replaceSpec{}, fmt.Errorf("payload path %q has no usable basename", localPath)
	case "init", "sandbox-init":
		return replaceSpec{imagePath: "/sbin/init", localPath: localPath}, nil
	default:
		return replaceSpec{
			imagePath: path.Join("/opt/sandbox-runtime/bin", base),
			localPath: localPath,
			upsert:    true,
		}, nil
	}
}

// looksLikeBundle reports whether path has the runtime bundle envelope
// shape: a regular 2 MiB-aligned file with EROFS magic at offset 1024.
func looksLikeBundle(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size()%pmemAlignment != 0 {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], erofsSuperOffset); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(magic[:]) == erofsMagic
}

type replaceSpec struct {
	imagePath string // absolute path inside the EROFS image, e.g. "/sbin/init"
	localPath string
	// upsert allows the target to be absent from the image (adding a new
	// file); set for positional payloads under /opt/sandbox-runtime/bin/.
	upsert bool
}

// replaceSpecs implements flag.Value for the repeatable -replace flag.
type replaceSpecs []replaceSpec

func (rs *replaceSpecs) String() string {
	parts := make([]string, len(*rs))
	for i, r := range *rs {
		parts[i] = r.imagePath + "=" + r.localPath
	}
	return strings.Join(parts, ",")
}

func (rs *replaceSpecs) Set(v string) error {
	imagePath, localPath, ok := strings.Cut(v, "=")
	if !ok || imagePath == "" || localPath == "" {
		return fmt.Errorf("-replace wants IMAGE-PATH=LOCAL-FILE, got %q", v)
	}
	*rs = append(*rs, replaceSpec{
		imagePath: path.Join("/", imagePath),
		localPath: localPath,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Bundle envelope
// ---------------------------------------------------------------------------

// footerIdentity reads and validates the trailing digest-marker ZIP, mirroring
// the envelope checks of internal/runtimebundle consumers, and returns the
// declared hex digest plus the prefix boundary (EROFS + padding) it covers.
func footerIdentity(f *os.File, size int64) (hexDigest string, prefixEnd int64, err error) {
	if size == 0 || size%pmemAlignment != 0 {
		return "", 0, fmt.Errorf("bundle size %d is not 2 MiB aligned", size)
	}
	var eocd [22]byte
	if _, err := f.ReadAt(eocd[:], size-int64(len(eocd))); err != nil {
		return "", 0, fmt.Errorf("read ZIP end record: %w", err)
	}
	if binary.LittleEndian.Uint32(eocd[:4]) != 0x06054b50 || binary.LittleEndian.Uint16(eocd[20:]) != 0 {
		return "", 0, fmt.Errorf("ZIP end record is not at EOF")
	}
	cdSize := int64(binary.LittleEndian.Uint32(eocd[12:16]))
	cdOffset := int64(binary.LittleEndian.Uint32(eocd[16:20]))
	commentLen := int64(binary.LittleEndian.Uint16(eocd[20:22]))
	// The marker ZIP is generated standalone and appended after the prefix,
	// so its declared central-directory offset is relative to the ZIP start,
	// not to the bundle file: prefix + local section + central directory +
	// EOCD (+ comment) = file size.
	prefixEnd = size - 22 - commentLen - cdSize - cdOffset
	if prefixEnd <= 1028 || cdOffset < 0 || cdSize <= 0 {
		return "", 0, fmt.Errorf("invalid digest ZIP bounds: prefix %d, central directory %d+%d", prefixEnd, cdOffset, cdSize)
	}

	zr, err := zip.NewReader(io.NewSectionReader(f, 0, size), size)
	if err != nil {
		return "", 0, fmt.Errorf("read digest ZIP: %w", err)
	}
	if len(zr.File) != 1 {
		return "", 0, fmt.Errorf("digest ZIP has %d entries, want 1", len(zr.File))
	}
	zf := zr.File[0]
	if zf.Method != zip.Store || zf.CompressedSize64 != 0 || zf.UncompressedSize64 != 0 {
		return "", 0, fmt.Errorf("digest marker %q is not empty", zf.Name)
	}
	hexDigest = strings.TrimPrefix(zf.Name, tarstream.DigestMarkerPrefix)
	if hexDigest == zf.Name || len(hexDigest) != 64 || strings.ToLower(hexDigest) != hexDigest {
		return "", 0, fmt.Errorf("invalid digest marker %q", zf.Name)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", 0, fmt.Errorf("invalid digest marker %q: %w", zf.Name, err)
	}
	return hexDigest, prefixEnd, nil
}

// verifyPrefixHash recomputes the SHA-256 of [0, prefixEnd) and compares it
// with the digest declared in the footer. Consumers trust the marker without
// recomputing; a repack source must additionally prove its own integrity.
func verifyPrefixHash(f *os.File, prefixEnd int64, hexDigest string) error {
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, 0, prefixEnd), prefixEnd); err != nil {
		return fmt.Errorf("hash bundle prefix: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hexDigest {
		return fmt.Errorf("bundle prefix digest %s differs from footer marker %s", got, hexDigest)
	}
	return nil
}

// erofsGeometry parses the EROFS superblock and returns the block size and
// the exact end of the raw EROFS image inside the bundle prefix.
func erofsGeometry(f *os.File) (blockSize int, erofsEnd int64, err error) {
	var sb [64]byte
	if _, err := f.ReadAt(sb[:], erofsSuperOffset); err != nil {
		return 0, 0, fmt.Errorf("read EROFS superblock: %w", err)
	}
	if magic := binary.LittleEndian.Uint32(sb[0:4]); magic != erofsMagic {
		return 0, 0, fmt.Errorf("bad EROFS magic 0x%08X at offset %d", magic, erofsSuperOffset)
	}
	blkBits := sb[erofsOffBlkBits]
	if blkBits < 9 || blkBits > 16 {
		return 0, 0, fmt.Errorf("implausible EROFS blkszbits %d", blkBits)
	}
	blocks := binary.LittleEndian.Uint32(sb[erofsOffBlocks : erofsOffBlocks+4])
	return 1 << blkBits, int64(uint64(blocks) << blkBits), nil
}

// openImage opens the raw EROFS prefix of the bundle as an fs.FS.
func openImage(f *os.File, erofsEnd int64) (fs.FS, error) {
	img, err := erofs.Open(io.NewSectionReader(f, 0, erofsEnd))
	if err != nil {
		return nil, fmt.Errorf("open EROFS image: %w", err)
	}
	return img, nil
}

// ---------------------------------------------------------------------------
// Replacement overlay
// ---------------------------------------------------------------------------

// payloadMode is the publish mode for every payload entry.
const payloadMode = fs.FileMode(0o777)

// replacement serves one image path from a local file instead of the source
// image. Payload entries are published with mode 0777, root ownership, and
// zero mtime — the all-root, zero-timestamp image conventions — regardless
// of what the source image or the local file carried.
type replacement struct {
	imagePath string
	localPath string
	localSize int64
	localSHA  string
	added     bool  // target absent from the source image (upsert)
	ino       int64 // synthetic inode, assigned deterministically
}

// prepare validates each spec against the source image and the local file,
// and computes the digests reported at the end of a repack. Spec targets
// must exist in the image unless the spec is an upsert, which may add a new
// file.
func prepare(img fs.FS, specs replaceSpecs) (map[string]*replacement, error) {
	out := make(map[string]*replacement, len(specs))
	for _, spec := range specs {
		r := &replacement{imagePath: spec.imagePath, localPath: spec.localPath}
		info, err := fs.Stat(img, strings.TrimPrefix(spec.imagePath, "/"))
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: image entry is not a regular file", spec.imagePath)
			}
			st, _ := info.Sys().(*erofs.Stat)
			if st == nil {
				return nil, fmt.Errorf("%s: image stat carries no EROFS metadata", spec.imagePath)
			}
			if st.Nlink > 1 {
				return nil, fmt.Errorf("%s: image entry has nlink %d; hard-linked entries are not supported", spec.imagePath, st.Nlink)
			}
		case spec.upsert && errors.Is(err, fs.ErrNotExist):
			r.added = true
		default:
			return nil, fmt.Errorf("%s: %w (only /opt/sandbox-runtime/bin/ payloads may add new files)", spec.imagePath, err)
		}
		lf, err := os.Open(spec.localPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", spec.imagePath, err)
		}
		h := sha256.New()
		localSize, err := io.Copy(h, lf)
		closeErr := lf.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: hash %s: %w", spec.imagePath, spec.localPath, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%s: close %s: %w", spec.imagePath, spec.localPath, closeErr)
		}
		lstat, err := os.Stat(spec.localPath)
		if err != nil || !lstat.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: %s is not a regular local file", spec.imagePath, spec.localPath)
		}
		r.localSize = localSize
		r.localSHA = hex.EncodeToString(h.Sum(nil))
		out[spec.imagePath] = r
	}
	return out, nil
}

// overlayFS serves the source image with the replacement files substituted
// at their image paths. It implements the interfaces the EROFS writer's
// CopyFrom probes for: fs.FS, fs.ReadDirFS, readLinker, and blockSizer.
type overlayFS struct {
	img       fs.FS
	repls     map[string]*replacement
	blockSize int
}

func (o *overlayFS) abs(name string) string {
	return path.Join("/", name)
}

func (o *overlayFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if r, ok := o.repls[o.abs(name)]; ok {
		lf, err := os.Open(r.localPath)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &replFile{f: lf, info: r.fileInfo(path.Base(name))}, nil
	}
	return o.img.Open(name)
}

func (o *overlayFS) ReadDir(name string) ([]fs.DirEntry, error) {
	rd, ok := o.img.(fs.ReadDirFS)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	entries, err := rd.ReadDir(name)
	if err != nil {
		return nil, err
	}
	dirKey := o.abs(name)
	for i, e := range entries {
		if r, ok := o.repls[path.Join(dirKey, e.Name())]; ok {
			entries[i] = &replEntry{base: e.Name(), info: r.fileInfo(e.Name())}
		}
	}
	// Upserts absent from the image join the listing of their parent
	// directory (/opt/sandbox-runtime/bin/ always exists in practice).
	for p, r := range o.repls {
		if r.added && path.Dir(p) == dirKey {
			entries = append(entries, &replEntry{base: path.Base(p), info: r.fileInfo(path.Base(p))})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (o *overlayFS) ReadLink(name string) (string, error) {
	if _, ok := o.repls[o.abs(name)]; ok {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	type readLinker interface{ ReadLink(string) (string, error) }
	if rl, ok := o.img.(readLinker); ok {
		return rl.ReadLink(name)
	}
	return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
}

// BlockSize propagates the source image block size to the writer.
func (o *overlayFS) BlockSize() int { return o.blockSize }

// fileInfo synthesizes the FileInfo CopyFrom sees for a payload entry: the
// size comes from the local file, the mode is 0777, and ownership and mtime
// follow the all-root, zero-timestamp image conventions. Inode numbers are
// synthetic, assigned by runRepack above the source image's nid range.
func (r *replacement) fileInfo(name string) fs.FileInfo {
	return &replFileInfo{
		name: name,
		mode: payloadMode,
		size: r.localSize,
		mod:  time.Unix(0, 0),
		stat: &erofs.Stat{
			Mode:    payloadMode,
			Ino:     r.ino,
			Nlink:   1,
			Mtime:   0,
			MtimeNs: 0,
		},
	}
}

type replFileInfo struct {
	name string
	mode fs.FileMode
	size int64
	mod  time.Time
	stat *erofs.Stat
}

func (fi *replFileInfo) Name() string       { return fi.name }
func (fi *replFileInfo) Size() int64        { return fi.size }
func (fi *replFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *replFileInfo) ModTime() time.Time { return fi.mod }
func (fi *replFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *replFileInfo) Sys() any           { return fi.stat }

// replFile streams the local file content while reporting the synthesized
// metadata.
type replFile struct {
	f    *os.File
	info fs.FileInfo
}

func (rf *replFile) Stat() (fs.FileInfo, error) { return rf.info, nil }
func (rf *replFile) Read(p []byte) (int, error) { return rf.f.Read(p) }
func (rf *replFile) Close() error               { return rf.f.Close() }

// replEntry swaps a directory listing entry for a replaced path.
type replEntry struct {
	base string
	info fs.FileInfo
}

func (e *replEntry) Name() string { return e.base }
func (e *replEntry) IsDir() bool  { return false }
func (e *replEntry) Type() fs.FileMode {
	return e.info.Mode() & fs.ModeType
}
func (e *replEntry) Info() (fs.FileInfo, error) { return e.info, nil }

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

type entrySummary struct {
	path  string
	isDir bool
	mode  fs.FileMode
	uid   uint32
	gid   uint32
	size  int64
	sha   string
	link  string
}

// summarize walks an image and records per-entry metadata plus the SHA-256 of
// every regular file, keyed by the absolute image path.
func summarize(fsys fs.FS) (map[string]entrySummary, error) {
	type readLinker interface{ ReadLink(string) (string, error) }
	rl, hasRL := fsys.(readLinker)

	out := map[string]entrySummary{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		s := entrySummary{path: "/" + p, isDir: info.IsDir(), mode: info.Mode()}
		if st, ok := info.Sys().(*erofs.Stat); ok {
			s.uid, s.gid = st.UID, st.GID
		}
		switch {
		case info.Mode().IsRegular():
			s.size = info.Size()
			f, err := fsys.Open(p)
			if err != nil {
				return fmt.Errorf("open %s: %w", p, err)
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("read %s: %w", p, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %s: %w", p, closeErr)
			}
			s.sha = hex.EncodeToString(h.Sum(nil))
		case info.Mode()&fs.ModeSymlink != 0 && hasRL:
			s.link, err = rl.ReadLink(p)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", p, err)
			}
		}
		out[s.path] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// verify compares the rebuilt image against the source image and the local
// payload files: every source entry must survive with identical metadata and
// bytes unless it was a payload target; every payload entry must carry the
// local file's bytes with mode 0777 and root ownership; nothing else may be
// added, dropped, or changed.
func verify(old, new map[string]entrySummary, repls map[string]*replacement) error {
	for p, oe := range old {
		ns, ok := new[p]
		if !ok {
			return fmt.Errorf("entry %s is missing from the rebuilt bundle", p)
		}
		if _, isPayload := repls[p]; isPayload {
			continue // checked against the local file below
		}
		if oe.isDir != ns.isDir || oe.mode != ns.mode || oe.uid != ns.uid || oe.gid != ns.gid {
			return fmt.Errorf("%s: metadata changed (mode %v->%v, uid %d->%d, gid %d->%d)",
				p, oe.mode, ns.mode, oe.uid, ns.uid, oe.gid, ns.gid)
		}
		if oe.sha != ns.sha || oe.size != ns.size {
			return fmt.Errorf("%s: content changed (size %d -> %d)", p, oe.size, ns.size)
		}
		if oe.link != ns.link {
			return fmt.Errorf("%s: symlink target changed (%q -> %q)", p, oe.link, ns.link)
		}
	}
	for p, r := range repls {
		ns, ok := new[p]
		if !ok {
			return fmt.Errorf("%s: missing from the rebuilt bundle", p)
		}
		if !ns.mode.IsRegular() {
			return fmt.Errorf("%s: rebuilt entry is not a regular file", p)
		}
		if ns.sha != r.localSHA {
			return fmt.Errorf("%s: rebuilt content %s differs from local file %s", p, ns.sha[:12], r.localSHA[:12])
		}
		if ns.mode != payloadMode || ns.uid != 0 || ns.gid != 0 {
			return fmt.Errorf("%s: payload entries must be mode 0777 root:root (got mode %v, uid %d, gid %d)",
				p, ns.mode, ns.uid, ns.gid)
		}
	}
	for p := range new {
		if _, ok := old[p]; !ok {
			if _, isPayload := repls[p]; !isPayload {
				return fmt.Errorf("entry %s was added to the rebuilt bundle", p)
			}
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

func runInspect(bundlePath string) error {
	f, size, err := openBundle(bundlePath)
	if err != nil {
		return err
	}
	defer f.Close()

	hexDigest, prefixEnd, err := footerIdentity(f, size)
	if err != nil {
		return err
	}
	if err := verifyPrefixHash(f, prefixEnd, hexDigest); err != nil {
		return err
	}
	blockSize, erofsEnd, err := erofsGeometry(f)
	if err != nil {
		return err
	}
	img, err := openImage(f, erofsEnd)
	if err != nil {
		return err
	}
	entries, err := summarize(img)
	if err != nil {
		return err
	}

	fmt.Printf("bundle    : %s\n", bundlePath)
	fmt.Printf("identity  : digest:%s (prefix hash verified)\n", hexDigest)
	fmt.Printf("erofs     : %d bytes, %d-byte blocks, prefix %d bytes\n", erofsEnd, blockSize, prefixEnd)
	fmt.Printf("entries   : %d\n", len(entries))
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := entries[p]
		switch {
		case e.isDir:
			fmt.Printf("  d  %s\n", p)
		case e.link != "":
			fmt.Printf("  l  %s -> %s\n", p, e.link)
		default:
			fmt.Printf("  f  %-12d %s  %s\n", e.size, short(e.sha), p)
		}
	}
	return nil
}

func runRepack(bundlePath, outputPath string, specs replaceSpecs) error {
	f, size, err := openBundle(bundlePath)
	if err != nil {
		return err
	}
	defer f.Close()

	hexDigest, prefixEnd, err := footerIdentity(f, size)
	if err != nil {
		return fmt.Errorf("source bundle %s: %w", bundlePath, err)
	}
	if err := verifyPrefixHash(f, prefixEnd, hexDigest); err != nil {
		return fmt.Errorf("source bundle %s: %w", bundlePath, err)
	}
	blockSize, erofsEnd, err := erofsGeometry(f)
	if err != nil {
		return err
	}
	img, err := openImage(f, erofsEnd)
	if err != nil {
		return err
	}

	repls, err := prepare(img, specs)
	if err != nil {
		return err
	}
	// Deterministic synthetic inode numbers for payload entries, far above
	// the source image's nid range so they cannot collide with real inodes.
	payloadPaths := make([]string, 0, len(repls))
	for p := range repls {
		payloadPaths = append(payloadPaths, p)
	}
	sort.Strings(payloadPaths)
	for i, p := range payloadPaths {
		repls[p].ino = 1<<32 + int64(i)
	}
	oldSum, err := summarize(img)
	if err != nil {
		return fmt.Errorf("summarize source image: %w", err)
	}

	// When the output lands on the source path (same file after path
	// resolution), keep the verified original as an identity-keyed backup so
	// a rollback point is never silently lost and a re-run with the same
	// inputs is a no-op for the backup.
	backup := ""
	if absIn, absOut := absPath(bundlePath), absPath(outputPath); absIn == absOut {
		backup = bundlePath + ".bak-" + hexDigest[:8]
		if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
			if err := copyVerified(f, size, backup); err != nil {
				return fmt.Errorf("keep source backup %s: %w", backup, err)
			}
		}
	}

	// Rebuild the EROFS: the release path packs with mkfs.erofs
	// --chunksize=4096 --all-root -T0 -b4096 -x-1 (chunked layout, root
	// ownership, zero timestamps, no xattrs). The pure-Go writer produces the
	// equivalent mount-compatible layout with flat-plain (non-inline) data —
	// the form required for DAX mapping of the pmem device — with the same
	// root ownership and zero timestamps, so the only differences the guest
	// can observe are the payload bytes themselves.
	raw, err := os.CreateTemp("", "runtime-repack-*.erofs")
	if err != nil {
		return fmt.Errorf("create raw EROFS: %w", err)
	}
	rawPath := raw.Name()
	defer func() { _ = os.Remove(rawPath) }()
	if err := raw.Close(); err != nil {
		return fmt.Errorf("close raw EROFS: %w", err)
	}

	ov := &overlayFS{img: img, repls: repls, blockSize: blockSize}
	if err := buildEROFS(rawPath, ov, blockSize); err != nil {
		return err
	}

	digest, err := runtimebundle.Build(rawPath, outputPath)
	if err != nil {
		return fmt.Errorf("wrap runtime bundle: %w", err)
	}

	// Verify the published file: reopen it, recheck the footer, and compare
	// every entry against the source image and the local replacement files.
	nf, nsize, err := openBundle(outputPath)
	if err != nil {
		return err
	}
	defer nf.Close()
	newDigest, newPrefixEnd, err := footerIdentity(nf, nsize)
	if err != nil {
		return fmt.Errorf("rebuilt bundle %s: %w", outputPath, err)
	}
	if newDigest != strings.TrimPrefix(digest, "digest:") {
		return fmt.Errorf("rebuilt bundle footer %s differs from build result %s", newDigest, digest)
	}
	if err := verifyPrefixHash(nf, newPrefixEnd, newDigest); err != nil {
		return fmt.Errorf("rebuilt bundle %s: %w", outputPath, err)
	}
	_, newErofsEnd, err := erofsGeometry(nf)
	if err != nil {
		return err
	}
	newImg, err := openImage(nf, newErofsEnd)
	if err != nil {
		return err
	}
	newSum, err := summarize(newImg)
	if err != nil {
		return fmt.Errorf("summarize rebuilt image: %w", err)
	}
	if err := verify(oldSum, newSum, repls); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	var files, dirs int
	for _, s := range newSum {
		if s.isDir {
			dirs++
		} else if s.sha != "" {
			files++
		}
	}
	fmt.Printf("source      : %s (digest:%s)\n", bundlePath, short(hexDigest))
	if backup != "" {
		fmt.Printf("output      : %s (digest:%s, in place — original kept at %s)\n", outputPath, short(newDigest), backup)
	} else {
		fmt.Printf("output      : %s (digest:%s)\n", outputPath, short(newDigest))
	}
	if len(repls) == 0 {
		fmt.Printf("payloads    : none (content re-encoded, identity refresh only)\n")
	} else {
		fmt.Printf("payloads    : (mode 0777, root:root)\n")
		for _, spec := range specs {
			r := repls[spec.imagePath]
			if r.added {
				fmt.Printf("  + %s  %d B  %s\n", r.imagePath, r.localSize, short(r.localSHA))
			} else {
				oe := oldSum[r.imagePath]
				fmt.Printf("  ~ %s  %d B %s -> %d B  %s\n", r.imagePath, oe.size, short(oe.sha), r.localSize, short(r.localSHA))
			}
		}
	}
	fmt.Printf("verified    : %d entries (%d files, %d dirs); all non-payload entries byte-identical\n",
		len(newSum), files, dirs)
	if newDigest != hexDigest {
		fmt.Printf("note        : identity changed — sandboxes snapshotted against the old bundle identity will not restore\n")
	}
	return nil
}

func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// copyVerified copies the open, already-verified bundle to dest, preserving
// its permission bits, and refuses to overwrite an existing file.
func copyVerified(f *os.File, size int64, dest string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(f, 0, size)); err != nil {
		out.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return nil
}

func openBundle(bundlePath string) (*os.File, int64, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", bundlePath)
	}
	return f, st.Size(), nil
}

func buildEROFS(rawPath string, src fs.FS, blockSize int) error {
	out, err := os.OpenFile(rawPath, os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open raw EROFS: %w", err)
	}
	w := erofs.Create(out,
		erofs.WithBlockSize(blockSize),
		erofs.WithBuildTime(0, 0),
		erofs.WithNoInlineData(),
	)
	if err := w.CopyFrom(src); err != nil {
		out.Close()
		return fmt.Errorf("copy image entries: %w", err)
	}
	if err := w.Close(); err != nil {
		out.Close()
		return fmt.Errorf("write EROFS: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close raw EROFS: %w", err)
	}
	return nil
}
