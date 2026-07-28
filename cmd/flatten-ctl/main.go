// flatten-ctl converts OCI / docker-archive images into deterministic
// EROFS rootfs images with an appended OCI runtime-config ZIP.
//
// Subcommands:
//
//	flatten-ctl export   [--output <path|->] [--config <path>]
//	                     [--manifest-config <path>] [--upload]  <path|->
//	flatten-ctl referer  lookup | put
//	flatten-ctl info     [--json] [--manifest-config <path>]  <path|manifest://hex>
//	flatten-ctl cache    gc | info
//	flatten-ctl config   [--config <path>] [--template] [-o <file>]
//
// The image is a positional arg (flags must precede it — stdlib flag).
// For export it defaults to `-` (docker-archive on stdin) when
// omitted; `-` may also be given explicitly.
//
// `export --upload` ingests the produced EROFS into the content store
// and prints the resulting manifest key on stdout (the EROFS itself
// is discarded unless --output is also given).
//
// `info` with a manifest:// reference fetches the EROFS via cache-ctl
// + store-ctl, then reads its superblock + ZIP trailer.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/flatten"
	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/remote"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/guest-runtime/internal/util"
)

const (
	manifestConfigEnv = "MANIFEST_CONFIG"
	flattenConfigEnv  = "FLATTEN_CONFIG"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "export":
		cmdExport(os.Args[2:])
	case "referer":
		cmdReferer(os.Args[2:])
	case "info":
		cmdInfo(os.Args[2:])
	case "cache":
		cmdCache(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	case "tar":
		cmdTar(os.Args[2:])
	case "mountpoint":
		cmdMountpoint(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: flatten-ctl <command> [flags]

Commands:
  export   Flatten OCI/docker-archive or a registry image → EROFS (optionally upload).
  referer  Lookup or write OCI Referrers records for flattened image manifests.
  info     Print EROFS metadata + OCI runtime config.
  cache    Inspect or garbage-collect the registry blob cache.
  config   Emit/validate a flatten config (FLATTEN_CONFIG): tmpdir/platform/cache/referer.
  tar      Extract files from a tar stream / package one sparse file as a tarstream (pure Go).
  mountpoint  Make a directory a self-bind mount point (excluded by export --skip-mounts).

See `+"`flatten-ctl <command> -h`"+` for per-command flags.
`)
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

func cmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	output := fs.String("output", "", "EROFS output path (- for stdout, empty = required with --upload off)")
	configPath := fs.String("config", "", "flatten config YAML (overrides FLATTEN_CONFIG env): tmpdir/platform/cache/referer (registry sources)")
	platform := fs.String("platform", "", "override the pull platform (os/arch[/variant]) from the config")
	manifestCfg := fs.String("manifest-config", "", "manifest config YAML (overrides MANIFEST_CONFIG env)")
	upload := fs.Bool("upload", false, "after flatten, ingest the EROFS into the store and print the manifest key on stdout")
	noProgress := fs.Bool("no-progress", false, "suppress progress output")
	printDigest := fs.Bool("print-digest", false, "print the resolved source image digest (repo@sha256:...) on stdout (registry sources; incompatible with --output -)")
	forceRegistry := fs.Bool("registry", false, "force the positional arg to be a registry reference")
	forceArchive := fs.Bool("archive", false, "force the positional arg to be a local docker-archive")
	var skips stringList
	fs.Var(&skips, "skip", "rootfs-dir source: exclude this path (relative to the rootfs, node and subtree); repeatable")
	skipMounts := fs.Bool("skip-mounts", false, "rootfs-dir source: exclude every mount point under the rootfs")
	runtimeConfig := fs.String("runtime-config", "", "rootfs-dir source: runtime config JSON to append (OCI image config or a projected config.json)")
	tmpDir := fs.String("tmpdir", "", "scratch directory (overrides the config tmpdir; created if missing)")
	insecure := fs.Bool("insecure", false, "registry source: allow plain-HTTP / skip-TLS registries (overrides the config)")
	fs.Parse(args)
	input := fs.Arg(0)
	if input == "" {
		input = "-" // default: docker-archive stream on stdin
	}
	dirSrc := false
	if st, err := os.Stat(input); err == nil && st.IsDir() {
		dirSrc = true
	}

	cfg, err := remote.LoadConfig(*configPath, flattenConfigEnv)
	if err != nil {
		fatal("%v", err)
	}
	if err := cfg.SetPlatform(*platform); err != nil {
		fatal("%v", err)
	}
	if *tmpDir != "" {
		cfg.TmpDir = *tmpDir
	}
	if *insecure {
		cfg.Insecure = true
	}
	if cfg.TmpDir != "" {
		if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
			fatal("tmpdir: %v", err)
		}
	}
	if !*upload && (*output == "" || *output == "/dev/null") {
		fatal("--output is required when --upload is not set")
	}
	if *forceRegistry && *forceArchive {
		fatal("--registry and --archive are mutually exclusive")
	}
	if dirSrc {
		for flagName, set := range map[string]bool{
			"--registry": *forceRegistry, "--archive": *forceArchive,
			"--print-digest": *printDigest,
			"--platform":     *platform != "",
		} {
			if set {
				fatal("%s does not apply to a rootfs-directory source", flagName)
			}
		}
	} else if len(skips) > 0 || *skipMounts || *runtimeConfig != "" {
		fatal("--skip / --skip-mounts / --runtime-config apply only to a rootfs-directory source")
	}
	remoteSrc := !dirSrc && isRemoteSource(input, *forceRegistry, *forceArchive)
	if *printDigest && !remoteSrc {
		fatal("--print-digest applies only to registry sources")
	}
	if *printDigest && *output == "-" {
		fatal("--print-digest is incompatible with --output - (both write stdout)")
	}

	// Preserving the source image's file ownership needs root/CAP_CHOWN
	// (applyOwnerMode); check it up front so an unprivileged run fails fast
	// here instead of partway through the first layer's chown — after a
	// potentially expensive pull + extract. A rootfs-directory source never
	// chowns (mkfs.erofs records the source inodes' ownership as read), so
	// it only needs read access to the tree.
	if !dirSrc {
		if err := flatten.RequireOwnershipCap(); err != nil {
			fatal("%v", err)
		}
	}

	// The raw erofs always lands in a scratch file first (mkfs.erofs
	// needs a seekable output), then gets packed into the tarstream
	// artifact (entry "image") — the platform container for images —
	// whether the destination is a file or stdout.
	rawTmp, err := os.CreateTemp(cfg.TmpDir, "flatten-erofs-*.img")
	if err != nil {
		fatal("create temp output: %v", err)
	}
	rawTmp.Close()
	rawPath := rawTmp.Name()
	defer os.Remove(rawPath)

	switch {
	case dirSrc:
		if err := runDirFlatten(input, rawPath, cfg.TmpDir, skips, *skipMounts, *runtimeConfig, *noProgress); err != nil {
			fatal("%v", err)
		}
	case remoteSrc:
		if err := runRemoteFlatten(input, rawPath, cfg, *printDigest, *noProgress); err != nil {
			fatal("%v", err)
		}
	default:
		if err := runFlatten(input, rawPath, cfg.TmpDir, *noProgress); err != nil {
			fatal("%v", err)
		}
	}

	if !*noProgress {
		if info, err := os.Stat(rawPath); err == nil {
			fmt.Fprintf(os.Stderr, "EROFS image: %s\n", formatSize(info.Size()))
		}
	}

	// Pack the artifact. --output - streams it; --upload without
	// --output packs to a scratch artifact for ingest.
	artifactPath := *output
	switch *output {
	case "-":
		if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			fatal("export: refusing to write an image artifact to a terminal (use --output FILE or redirect stdout)")
		}
		artifactPath = ""
		if *upload { // need a file for ingest too: pack once, copy to stdout
			af, err := os.CreateTemp(cfg.TmpDir, "flatten-image-*.img")
			if err != nil {
				fatal("create temp artifact: %v", err)
			}
			af.Close()
			artifactPath = af.Name()
			defer os.Remove(artifactPath)
		}
		if artifactPath == "" {
			if err := packImageArtifact(rawPath, os.Stdout); err != nil {
				fatal("%v", err)
			}
		}
	case "":
		af, err := os.CreateTemp(cfg.TmpDir, "flatten-image-*.img")
		if err != nil {
			fatal("create temp artifact: %v", err)
		}
		af.Close()
		artifactPath = af.Name()
		defer os.Remove(artifactPath)
	}
	if artifactPath != "" {
		af, err := os.Create(artifactPath)
		if err != nil {
			fatal("create artifact: %v", err)
		}
		if err := packImageArtifact(rawPath, af); err != nil {
			af.Close()
			fatal("%v", err)
		}
		if err := af.Close(); err != nil {
			fatal("close artifact: %v", err)
		}
		if *output == "-" {
			f, err := os.Open(artifactPath)
			if err != nil {
				fatal("re-open artifact: %v", err)
			}
			if _, err := io.Copy(os.Stdout, f); err != nil {
				f.Close()
				fatal("write to stdout: %v", err)
			}
			f.Close()
		}
	}

	if !*upload {
		return
	}

	mcfg := loadManifestCfg(*manifestCfg)
	key, err := ingestEROFS(artifactPath, mcfg, *noProgress)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println(key)
}

// packImageArtifact wraps the raw erofs(+config ZIP) at rawPath as a
// tarstream artifact (payload "image" + digest marker) on w: the platform
// container for images. Holes come from the filesystem (the raw file
// is the live scratch source); the artifact itself is dense and
// self-describing.
func packImageArtifact(rawPath string, w io.Writer) error {
	f, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("open erofs: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	holes, err := sparse.ProbeHoles(f)
	if err != nil {
		return fmt.Errorf("probe holes: %w", err)
	}
	src, err := sparse.NewSource(f, uint64(st.Size()), holes)
	if err != nil {
		return err
	}
	if _, err := tarstream.WriteTo(context.Background(), w, "image", src); err != nil {
		return fmt.Errorf("pack image artifact: %w", err)
	}
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runDirFlatten exports an already-flattened rootfs directory
// (flatten.BuildFromDir): mkfs.erofs reads the tree in place — no
// staging copy, no source mutation.
func runDirFlatten(root, outputPath, tmpDir string, skips []string, skipMounts bool, runtimeConfig string, noProgress bool) error {
	dopts := flatten.DirOptions{
		Skip:       skips,
		SkipMounts: skipMounts,
	}
	if !noProgress {
		dopts.Warnf = func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}
	if runtimeConfig != "" {
		data, err := os.ReadFile(runtimeConfig)
		if err != nil {
			return fmt.Errorf("--runtime-config: %w", err)
		}
		dopts.ConfigJSON = data
	}
	opts := flatten.Options{TmpDir: tmpDir, Progress: flattenProgress(!noProgress)}
	return flatten.BuildFromDir(root, outputPath, opts, dopts)
}

func runFlatten(image, outputPath, tmpDir string, noProgress bool) error {
	opts := flatten.Options{TmpDir: tmpDir, Progress: flattenProgress(!noProgress)}
	if image == "-" {
		return flatten.FlattenWith(os.Stdin, outputPath, opts)
	}
	image = strings.TrimPrefix(image, "docker-archive:")
	return flatten.FlattenFileWith(image, outputPath, opts)
}

// isRemoteSource decides whether the export positional arg names a
// remote registry reference (vs a local docker-archive). Precedence: --archive
// and stdin / `docker-archive:` force local; --registry forces remote; an
// existing on-disk file is local; otherwise anything that parses as a registry
// reference is remote. The force flags disambiguate the rare case of a local
// file literally named like `repo:tag`.
func isRemoteSource(input string, forceRegistry, forceArchive bool) bool {
	if forceArchive {
		return false
	}
	if input == "-" || strings.HasPrefix(input, "docker-archive:") {
		return false
	}
	if forceRegistry {
		return true
	}
	if st, err := os.Stat(input); err == nil && !st.IsDir() {
		return false
	}
	return remote.LooksLikeReference(input)
}

// runRemoteFlatten resolves a registry reference (pinning the platform-selected
// digest), pulls its blobs through the shared OCI-layout cache, and flattens
// them into outputPath via the same deterministic sink the docker-archive path
// uses. The resolved repo@sha256 is logged to stderr (unless --no-progress) and
// echoed to stdout when --print-digest is set.
func runRemoteFlatten(ref, outputPath string, cfg *remote.Config, printDigest, noProgress bool) error {
	ctx := context.Background()
	res, err := cfg.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if !noProgress {
		fmt.Fprintf(os.Stderr, "resolved: %s\n", res.Digest)
	}
	if printDigest {
		fmt.Println(res.Digest.String())
	}
	cache, cleanup, err := cfg.OpenCache()
	if err != nil {
		return err
	}
	defer cleanup()
	cfg.OnPullProgress = pullProgress(!noProgress)
	src, err := cfg.Pull(ctx, res, cache)
	if err != nil {
		return err
	}
	opts := flatten.Options{TmpDir: cfg.TmpDir, Progress: flattenProgress(!noProgress)}
	if err := flatten.Build(src, outputPath, opts); err != nil {
		return err
	}
	return cache.MaybeEvict()
}

// ingestEROFS uploads the EROFS at path into the content store via the
// manifest ingester and returns the hex manifest key.
func ingestEROFS(path string, mcfg *manifest.Config, noProgress bool) (string, error) {
	ing, err := mcfg.NewIngester(mcfg.IngestKeyFunc(), nil)
	if err != nil {
		return "", fmt.Errorf("ingester: %w", err)
	}
	defer ing.Close()

	src, err := fetch.OpenTarStream(path)
	if err != nil {
		return "", fmt.Errorf("re-open artifact for upload: %w", err)
	}
	defer src.Close()

	res, err := ing.Ingest(context.Background(), src, ingest.IngestOption{
		OnProgress: byteProgress(!noProgress, "erofs"),
	})
	if err != nil {
		return "", fmt.Errorf("ingest: %w", err)
	}
	if !noProgress {
		fmt.Fprintf(os.Stderr, "stored: %s (chunks stored=%d dedup=%d zero=%d)\n",
			formatSize(int64(res.StoredBytes)), res.StoredChunks, res.DedupChunks, res.ZeroChunks)
	}
	return hex.EncodeToString(res.ManifestKey[:]), nil
}

// ---------------------------------------------------------------------------
// referer
// ---------------------------------------------------------------------------

type refererLookupResult struct {
	Supported  bool   `json:"supported"`
	Subject    string `json:"subject"`
	Hit        bool   `json:"hit"`
	ManifestID string `json:"manifest_id,omitempty"`
}

type refererPutResult struct {
	Subject    string `json:"subject"`
	ManifestID string `json:"manifest_id"`
	Written    bool   `json:"written"`
}

func cmdReferer(args []string) {
	if len(args) < 1 {
		fatal("usage: flatten-ctl referer <lookup|put> [flags]")
	}
	switch args[0] {
	case "lookup":
		cmdRefererLookup(args[1:])
	case "put":
		cmdRefererPut(args[1:])
	default:
		fatal("unknown referer command %q", args[0])
	}
}

func cmdRefererLookup(args []string) {
	fs := flag.NewFlagSet("referer lookup", flag.ExitOnError)
	configPath := fs.String("config", "", "flatten config YAML (overrides FLATTEN_CONFIG env)")
	platform := fs.String("platform", "", "override the pull platform (os/arch[/variant]) from the config")
	owner := fs.String("owner", "", "precomputed owner annotation value")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	insecure := fs.Bool("insecure", false, "allow plain-HTTP / skip-TLS registries (overrides the config)")
	fs.Parse(args)
	ref := fs.Arg(0)
	if ref == "" || *owner == "" {
		fatal("usage: flatten-ctl referer lookup --owner <owner> [--json] <registry-ref>")
	}
	cfg, err := remote.LoadConfig(*configPath, flattenConfigEnv)
	if err != nil {
		fatal("%v", err)
	}
	if err := cfg.SetPlatform(*platform); err != nil {
		fatal("%v", err)
	}
	if *insecure {
		cfg.Insecure = true
	}
	ctx := context.Background()
	res, err := cfg.Resolve(ctx, ref)
	if err != nil {
		fatal("%v", err)
	}
	id, supported, hit, err := cfg.FindReferrerByOwner(ctx, res, *owner)
	if err != nil {
		fatal("%v", err)
	}
	out := refererLookupResult{
		Supported:  supported,
		Subject:    res.Digest.String(),
		Hit:        hit,
		ManifestID: id,
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fatal("write json: %v", err)
		}
		return
	}
	switch {
	case !supported:
		fmt.Printf("unsupported %s\n", out.Subject)
	case hit:
		fmt.Println(id)
	default:
		fmt.Printf("miss %s\n", out.Subject)
	}
}

func cmdRefererPut(args []string) {
	fs := flag.NewFlagSet("referer put", flag.ExitOnError)
	configPath := fs.String("config", "", "flatten config YAML (overrides FLATTEN_CONFIG env)")
	platform := fs.String("platform", "", "override the pull platform (os/arch[/variant]) from the config")
	owner := fs.String("owner", "", "precomputed owner annotation value")
	manifestID := fs.String("manifest-id", "", "64-hex manifest id to write into the referrer annotation")
	validity := fs.String("validity", "", "optional Go duration for referrer expiry")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	insecure := fs.Bool("insecure", false, "allow plain-HTTP / skip-TLS registries (overrides the config)")
	fs.Parse(args)
	ref := fs.Arg(0)
	if ref == "" || *owner == "" || *manifestID == "" {
		fatal("usage: flatten-ctl referer put --owner <owner> --manifest-id <hex> <subject-ref>")
	}
	cfg, err := remote.LoadConfig(*configPath, flattenConfigEnv)
	if err != nil {
		fatal("%v", err)
	}
	if err := cfg.SetPlatform(*platform); err != nil {
		fatal("%v", err)
	}
	if *insecure {
		cfg.Insecure = true
	}
	if *validity != "" {
		cfg.Referer.Validity = *validity
	}
	ctx := context.Background()
	res, err := cfg.Resolve(ctx, ref)
	if err != nil {
		fatal("%v", err)
	}
	if err := cfg.PutReferrerByOwner(ctx, res, *manifestID, *owner); err != nil {
		fatal("%v", err)
	}
	out := refererPutResult{Subject: res.Digest.String(), ManifestID: *manifestID, Written: true}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fatal("write json: %v", err)
		}
		return
	}
	fmt.Printf("written %s %s\n", out.Subject, out.ManifestID)
}

// ---------------------------------------------------------------------------
// info
// ---------------------------------------------------------------------------

func cmdInfo(args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	manifestCfg := fs.String("manifest-config", "", "manifest config YAML (overrides MANIFEST_CONFIG env); required for manifest:// inputs")
	fs.Parse(args)
	input := fs.Arg(0)
	if input == "" {
		fatal("usage: flatten-ctl info <erofs-path|manifest://hex> [--json] [--manifest-config <file>]")
	}

	if strings.HasPrefix(input, "manifest://") {
		hexKey := strings.TrimPrefix(input, "manifest://")
		cfg := loadManifestCfg(*manifestCfg)
		fc, err := cfg.NewFetcher(cfg.FetchKeyFunc())
		if err != nil {
			fatal("fetcher: %v", err)
		}
		defer fc.Close()
		key, err := manifest.ParseHexKey(hexKey)
		if err != nil {
			fatal("%v", err)
		}
		ctx := context.Background()
		stream, err := fc.Fetch(ctx, key)
		if err != nil {
			fatal("fetch manifest: %v", err)
		}
		defer stream.Close()
		// Read only the EROFS superblock + trailing ZIP directly over
		// the chunk-granular fetch path — no full materialization.
		size := int64(stream.Size())
		printInfo(fetch.NewReaderAt(ctx, stream, size), size, *asJSON)
		return
	}

	st, err := fetch.OpenTarStream(input)
	if err != nil {
		fatal("%v", err)
	}
	defer st.Close()
	ctx := context.Background()
	size := int64(st.Size())
	printInfo(fetch.NewReaderAt(ctx, st, size), size, *asJSON)
}

// printInfo reports the EROFS image size and embedded RuntimeConfig from
// ra. Both image.ReadEROFSSize and image.ReadConfig need only the
// superblock and the trailing ZIP, so ra may be a plain *os.File (local
// path) or a fetch-backed io.ReaderAt (manifest://) — the manifest case
// then transfers only those few KB, never the whole image.
func printInfo(ra io.ReaderAt, size int64, asJSON bool) {
	erofsSize, sbErr := image.ReadEROFSSize(ra)
	if sbErr != nil {
		fatal("read EROFS superblock: %v", sbErr)
	}
	cfg, cfgErr := image.ReadConfig(ra, size)
	if cfgErr != nil && !errors.Is(cfgErr, fs.ErrNotExist) {
		fatal("read config: %v", cfgErr)
	}

	if asJSON {
		out := struct {
			ErofsSize uint64               `json:"erofs_size"`
			Config    *image.RuntimeConfig `json:"config"`
		}{ErofsSize: erofsSize, Config: cfg}
		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fatal("marshal: %v", err)
		}
		fmt.Println(string(body))
		return
	}
	printInfoHuman(erofsSize, cfg)
}

func printInfoHuman(erofsSize uint64, cfg *image.RuntimeConfig) {
	fmt.Printf("EROFS image size:  %s (%d bytes)\n", formatSize(int64(erofsSize)), erofsSize)
	if cfg == nil {
		fmt.Println("(no OCI config trailer)")
		return
	}
	fmt.Printf("Architecture:      %s\n", emptyDash(cfg.Architecture))
	fmt.Printf("Os:                %s\n", emptyDash(cfg.Os))
	if cfg.User != "" {
		fmt.Printf("User:              %s\n", cfg.User)
	}
	if len(cfg.Entrypoint) > 0 {
		fmt.Printf("Entrypoint:        %s\n", jsonInline(cfg.Entrypoint))
	}
	if len(cfg.Cmd) > 0 {
		fmt.Printf("Cmd:               %s\n", jsonInline(cfg.Cmd))
	}
	if cfg.WorkingDir != "" {
		fmt.Printf("WorkingDir:        %s\n", cfg.WorkingDir)
	}
	if len(cfg.Env) > 0 {
		fmt.Printf("Env (%d):\n", len(cfg.Env))
		for _, e := range cfg.Env {
			fmt.Printf("  %s\n", e)
		}
	}
	if len(cfg.ExposedPorts) > 0 {
		fmt.Printf("ExposedPorts:      %s\n", strings.Join(sortedKeys(cfg.ExposedPorts), ", "))
	}
	if len(cfg.Volumes) > 0 {
		fmt.Printf("Volumes:           %s\n", strings.Join(sortedKeys(cfg.Volumes), ", "))
	}
	if cfg.StopSignal != "" {
		fmt.Printf("StopSignal:        %s\n", cfg.StopSignal)
	}
	if len(cfg.Labels) > 0 {
		fmt.Println("Labels:")
		keys := make([]string, 0, len(cfg.Labels))
		for k := range cfg.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s=%s\n", k, cfg.Labels[k])
		}
	}
	if cfg.Healthcheck != nil {
		fmt.Printf("Healthcheck:       Test=%s Interval=%dns Retries=%d\n",
			jsonInline(cfg.Healthcheck.Test), cfg.Healthcheck.Interval, cfg.Healthcheck.Retries)
	}
}

func emptyDash(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func jsonInline(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// cache
// ---------------------------------------------------------------------------

func cmdCache(args []string) {
	if len(args) == 0 {
		fatal("usage: flatten-ctl cache <gc|info> [flags]")
	}
	switch args[0] {
	case "gc":
		cmdCacheGC(args[1:])
	case "info":
		cmdCacheInfo(args[1:])
	case "-h", "--help":
		fmt.Fprintln(os.Stderr, "usage: flatten-ctl cache <gc|info> [flags]")
	default:
		fatal("unknown cache subcommand %q (want gc|info)", args[0])
	}
}

// openCacheFromFlags resolves the cache directory and size cap from the remote
// config plus optional overrides, then opens the OCI-layout cache.
func openCacheFromFlags(remoteCfgPath, cacheDirOverride, maxSizeOverride string) (*remote.Cache, error) {
	cfg, err := remote.LoadConfig(remoteCfgPath, flattenConfigEnv)
	if err != nil {
		return nil, err
	}
	dir := cfg.CacheDir()
	if cacheDirOverride != "" {
		dir = cacheDirOverride
	}
	maxSize := cfg.MaxCacheBytes()
	if maxSizeOverride != "" {
		v, err := util.ParseSize(maxSizeOverride)
		if err != nil {
			return nil, fmt.Errorf("--cache-max-size: %w", err)
		}
		maxSize = int64(v)
	}
	return remote.OpenCache(dir, maxSize)
}

func cmdCacheInfo(args []string) {
	fs := flag.NewFlagSet("cache info", flag.ExitOnError)
	remoteCfg := fs.String("config", "", "flatten config YAML (overrides FLATTEN_CONFIG env)")
	cacheDir := fs.String("cache-dir", "", "cache directory (overrides config)")
	fs.Parse(args)

	cache, err := openCacheFromFlags(*remoteCfg, *cacheDir, "")
	if err != nil {
		fatal("%v", err)
	}
	st, err := cache.Stats()
	if err != nil {
		fatal("%v", err)
	}
	maxStr := "unlimited"
	if st.MaxSize > 0 {
		maxStr = formatSize(st.MaxSize)
	}
	fmt.Printf("Cache dir:   %s\n", st.Dir)
	fmt.Printf("Blobs:       %d\n", st.Blobs)
	fmt.Printf("Total size:  %s (%d bytes)\n", formatSize(st.TotalSize), st.TotalSize)
	fmt.Printf("Max size:    %s\n", maxStr)
}

func cmdCacheGC(args []string) {
	fs := flag.NewFlagSet("cache gc", flag.ExitOnError)
	remoteCfg := fs.String("config", "", "flatten config YAML (overrides FLATTEN_CONFIG env)")
	cacheDir := fs.String("cache-dir", "", "cache directory (overrides config)")
	maxSize := fs.String("cache-max-size", "", "evict LRU blobs down to this size (overrides config; \"0\" = evict all eligible)")
	noProgress := fs.Bool("no-progress", false, "suppress progress output")
	fs.Parse(args)

	cache, err := openCacheFromFlags(*remoteCfg, *cacheDir, *maxSize)
	if err != nil {
		fatal("%v", err)
	}
	// Evict down to the (possibly overridden) configured cap. Stats reports
	// the resolved cap so we evict to exactly that target.
	st, err := cache.Stats()
	if err != nil {
		fatal("%v", err)
	}
	freed, err := cache.Evict(st.MaxSize)
	if err != nil {
		fatal("%v", err)
	}
	if !*noProgress {
		fmt.Fprintf(os.Stderr, "evicted: %s\n", formatSize(freed))
	}
}

// ---------------------------------------------------------------------------
// Common helpers
// ---------------------------------------------------------------------------

func loadManifestCfg(flagPath string) *manifest.Config {
	cfg, err := manifest.LoadConfig(flagPath, manifestConfigEnv)
	if err != nil {
		if errors.Is(err, manifest.ErrConfigNotProvided) {
			fatal("missing manifest config: pass --manifest-config <path> or set %s", manifestConfigEnv)
		}
		fatal("load config: %v", err)
	}
	return cfg
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func hashAndSize(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), info.Size(), nil
}

func formatSize(bytes int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	case bytes >= kib:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(kib))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
