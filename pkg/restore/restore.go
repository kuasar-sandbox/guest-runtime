// Package restore implements the `sandbox-ctl run --restore=` lifecycle.
// Reads a <sid>.snapshot bundle (memory + ZIP at end with config.json /
// state.json / snapshot.cfg), prepares memfd + va_report server, spawns
// patched CH with --restore source_url pointing at a temp dir holding
// the rewritten state.json, and lets faults flow.
package restore

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/config"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/guestlink"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/resctl"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/sandbox-accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/chapi"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/proto"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/stdio"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/tapfd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/uffd"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/vhost"
)

// Options is the restore-specific input.
//
// Snapshot can be supplied either as a local file path or as a
// manifest:// URI. When a manifest:// URI is given, Fetcher must
// be non-nil and the bundle is read via fetch.Fetcher (chunk-granular,
// cache-ctl backed); the uffd source becomes ManifestSnapshotSource
// instead of SparseSnapshotSource.
//
// blk0 / overlay.base in the embedded sandbox.cfg likewise support
// manifest:// when Fetcher is set.
type Options struct {
	SnapshotPath        string                 // file path; mutually exclusive with SnapshotManifestKey
	SnapshotManifestKey string                 // hex content key; mutually exclusive with SnapshotPath
	HostCfg             *config.SandboxConfig  // host yaml: TAP, blk1.diff, etc.
	ManifestCfg         *config.ManifestConfig // for snapshot --upload from a restored sandbox
	Fetcher             fetch.Fetcher          // required when any URI is manifest://; caller owns lifecycle
	SandboxID           string
	CHBinary            string
	RuntimeRoot         string        // tmpfs run root; "/run/sandbox" by default
	BaseRoot            string        // on-disk base root (fresh overlay diff); "/var/lib/sandbox" by default
	StatsJSONPath       string        // if non-empty, dump uffd + per-backend stats here on exit
	StatsInterval       time.Duration // if > 0, periodically log lazy-load stats; 0 = off
	StdioMode           stdio.Mode    // CH process stdio wiring; see pkg/stdio

	// PingFatalThreshold: same semantics as sandbox.RunOptions —
	// SIGTERM CH after N consecutive ping failures. 0 disables.
	PingFatalThreshold int

	// Forwards are parsed `--connect` port-forward directives — a restored
	// sandbox re-opens the same host-local listeners (forward.go). Empty →
	// no port forwarding.
	Forwards []sandbox.ForwardSpec
}

// Run executes restore. Returns the CH exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.SnapshotPath == "" && opts.SnapshotManifestKey == "" {
		return -1, errors.New("restore: SnapshotPath or SnapshotManifestKey required")
	}
	if opts.SnapshotPath != "" && opts.SnapshotManifestKey != "" {
		return -1, errors.New("restore: SnapshotPath and SnapshotManifestKey are mutually exclusive")
	}
	if opts.SnapshotManifestKey != "" && opts.Fetcher == nil {
		return -1, errors.New("restore: manifest:// snapshot requires Fetcher")
	}
	if opts.HostCfg == nil {
		return -1, errors.New("restore: HostCfg required")
	}
	if opts.SandboxID == "" {
		opts.SandboxID = "rs-default"
	}
	if opts.RuntimeRoot == "" {
		opts.RuntimeRoot = "/run/sandbox"
	}
	if opts.BaseRoot == "" {
		opts.BaseRoot = sandbox.DefaultBaseRoot
	}
	if opts.CHBinary == "" {
		opts.CHBinary = "cloud-hypervisor"
	}

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl run --restore] "+format, a...) }
	startUnixNs := time.Now().UnixNano()

	// cgroup join (same semantics as cold-start lifecycle.go). No-cgroup
	// mode (no cgroup_path) is a no-op. See docs/sandbox.md §4.1.
	// Initial memory.high uses the configured allocatable; the value gets
	// bumped after we derive allocatable_at_snapshot from the bundle's
	// state.json balloon (below).
	cg, err := resctl.JoinCgroupForConfig(opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.Active() {
		logf("cgroup limits set: %s (CH joins on start; sandbox-ctl stays out)", cg.Path)
	}
	defer func() { _ = cg.Cleanup() }()

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, err
	}
	defer os.RemoveAll(runDir)

	chSock := filepath.Join(runDir, "ch.sock")
	blk0Sock := filepath.Join(runDir, "blk0.sock")
	blk1Sock := filepath.Join(runDir, "blk1.sock")
	uffdSock := filepath.Join(runDir, "uffd.sock")
	stateDir := filepath.Join(runDir, "snap-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return -1, err
	}

	// Restore-side controller hooks. Admit happens once we've derived
	// allocatable_at_snapshot from the bundle's state.json balloon section
	// (see deriveAllocatableAtSnapshot below).
	// Balloon is created later (snapCap unknown until snapCfg is parsed),
	// then late-injected via hooks.SetBalloon. Until then, hooks balloon-
	// related entry points (SettledRestore, OnAllocatableChanged) treat
	// Balloon-nil as no-op on the balloon side.
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: opts.HostCfg.Resources.Control.Controller,
		CgroupPath: opts.HostCfg.Resources.Control.CgroupPath,
		Logf:       logf,
	}, opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	defer hooks.Release("normal")

	// Open the snapshot bundle as a single fetch.Stream — file:// is a
	// sparse-aware local stream, manifest:// is chunk-granular via cache-ctl.
	// The same Stream feeds the ZIP reader (via NewReaderAt) and, layered with
	// from_refs (§3.5), the uffd SnapshotReader. selfRef is this bundle's
	// content-addressed identity, recorded into a child snapshot's from_refs.
	var (
		selfStream   fetch.Stream
		snapReaderAt io.ReaderAt
		totalSize    int64
		selfRef      string
	)
	if opts.SnapshotPath != "" {
		fs, err := fetch.OpenFileStream(opts.SnapshotPath)
		if err != nil {
			return -1, fmt.Errorf("open snapshot: %w", err)
		}
		defer fs.Close()
		selfStream = fs
		totalSize = int64(fs.Size())
		selfRef = fileSnapshotRef(opts.SnapshotPath) // §3.5: follows symlink → file://<sha256>.snapshot
	} else {
		fc, sz, err := sandbox.OpenManifestStream(ctx, opts.SnapshotManifestKey, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open manifest snapshot: %w", err)
		}
		defer fc.Close()
		selfStream = fc
		totalSize = sz
		selfRef = "manifest://" + opts.SnapshotManifestKey
		logf("manifest snapshot: key=%s bundle_size=%d", opts.SnapshotManifestKey, sz)
	}
	snapReaderAt = fetch.NewReaderAt(ctx, selfStream, totalSize)

	zipReader, err := zip.NewReader(snapReaderAt, totalSize)
	if err != nil {
		return -1, fmt.Errorf("zip.NewReader on snapshot: %w", err)
	}
	entries := make(map[string][]byte)
	for _, f := range zipReader.File {
		rc, err := f.Open()
		if err != nil {
			return -1, fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return -1, fmt.Errorf("zip read %s: %w", f.Name, err)
		}
		entries[f.Name] = body
	}
	for _, want := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := entries[want]; !ok {
			return -1, fmt.Errorf("snapshot bundle missing %s (produced by old sandbox-ctl?)", want)
		}
	}

	// snapshot.cfg carries the post-quiesce platform contract: capacity,
	// runtime_ref, base_ref, overlay.base. ApplyRules merges it with the
	// host sandbox.yaml per docs/sandbox.md §11.0 — capacity must match
	// exactly when host provides it, runtime/base are validated against
	// digest, network.tap is required, overlay.diff is required.
	parsedSnap, err := ParseSnapshotCfg(entries["snapshot.cfg"])
	if err != nil {
		return -1, err
	}
	merged, err := ApplyRules(opts.HostCfg, parsedSnap, opts.SnapshotPath)
	if err != nil {
		return -1, err
	}
	snapCfg := *merged

	// Carry the runtime/base refs forward so a snapshot taken by this restored
	// run records them (the cold path hashes them via populateSnapshotRefs;
	// here they're already known + verified from the parent snapshot.cfg, so a
	// re-hash is unnecessary). Without this, snapshots from a restored sandbox
	// would have empty runtime_ref/base_ref and could not themselves be restored.
	snapCfg.SnapshotRefs = config.SnapshotRefs{
		RuntimeRef: parsedSnap.Boot.RuntimeRef,
		BaseRef:    parsedSnap.Boot.Root.BaseRef,
	}

	// Record provenance so a snapshot taken by this restored run prepends this
	// bundle and extends the chain (§3.5): child.from_refs = [selfRef] ++
	// this.from_refs; child.base_from_refs = [this.overlay.base] ++ this.base_from_refs.
	// The parent's "top disk layer" + chain below it: overlay.base/overlay
	// .base_from_refs in overlay mode, root.base/root.base_from_refs in
	// single-disk mode (the captured diff is recorded at root level there).
	parentDiskBase, parentDiskChain := parsedSnap.Boot.Root.Base, parsedSnap.Boot.Root.BaseFromRefs
	if !parsedSnap.SingleDisk() {
		parentDiskBase = parsedSnap.Boot.Root.Overlay.Base
		parentDiskChain = parsedSnap.Boot.Root.Overlay.BaseFromRefs
	}
	snapCfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef:  selfRef,
		ParentFromRefs:     parsedSnap.FromRefs,
		ParentOverlayBase:  parentDiskBase,
		ParentBaseFromRefs: parentDiskChain,
	}
	// Local restore: record the parent's on-disk bundle + top-disk-layer paths
	// so a re-export merges this run's resident delta onto them (replace the
	// next-newest local layer, not stack a second one) — docs §3.5. Paths
	// resolve like openRefStream: the disk layer is a basename in the bundle dir.
	if opts.SnapshotPath != "" {
		if abs, err := filepath.Abs(opts.SnapshotPath); err == nil {
			snapCfg.SnapshotProvenance.ParentSnapshotPath = abs
		}
		if sc, val, ok := config.SchemeAndPath(parentDiskBase); ok && sc == "file" {
			if !filepath.IsAbs(val) {
				val = filepath.Join(filepath.Dir(opts.SnapshotPath), val)
			}
			snapCfg.SnapshotProvenance.ParentOverlayPath = val
		}
	}

	// Derive allocatable_at_snapshot from CH state.json's balloon section
	// (no separate resource-state.json file — see §13). When the bundle
	// predates balloon use or balloon was disabled, parseBalloonFromState
	// returns ok=false and we fall back to yaml.allocatable as if it were
	// a cold start.
	snapCap, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("snap sandbox.cfg capacity: %w", err)
	}
	balTarget, balCurrent, balOk, err := parseBalloonFromState(entries["state.json"])
	if err != nil {
		return -1, fmt.Errorf("parse balloon from state.json: %w", err)
	}
	allocAtSnap := deriveAllocatableAtSnapshot(snapCap, balTarget, balCurrent, balOk)

	// BalloonController, sole writer of /vm.resize. Created once we know
	// snapCap and allocAtSnap: target is seeded to `cap - allocAtSnap` so
	// the in-memory state matches what CH will load from state.json when
	// it starts with --restore. Subsequent SettledRestore decides whether
	// a runtime correction is needed (initialAlloc != allocAtSnap).
	var balloonCtl *resctl.BalloonController
	if allocAtSnap < snapCap {
		balloonCtl = resctl.NewBalloonController(chSock, snapCap, logf)
		balloonCtl.SetAllocatable(allocAtSnap)
		hooks.SetBalloon(balloonCtl)
	}

	yamlAlloc, err := opts.HostCfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, err
	}

	// Static mode: take max(yaml, snapshot allocatable). When the snapshot
	// was captured under a controller (dynamic mode) at a burst-elevated
	// allocatable, restoring under static mode (A/B) preserves that
	// elevated working set rather than throttling the guest.
	initialAlloc := yamlAlloc
	if allocAtSnap > initialAlloc {
		initialAlloc = allocAtSnap
	}
	if hooks.Enabled() {
		// Dynamic mode: controller decides. Floor sent = yaml.allocatable
		// (controller's 2-tier fallback uses it if headroom can't fit
		// allocAtSnap).
		granted, err := hooks.Admit(opts.SandboxID, allocAtSnap)
		if err != nil {
			return -1, fmt.Errorf("controller admit: %w", err)
		}
		initialAlloc = granted
		logf("controller admit ok, restored allocatable=%d (snapshot allocatable=%d, balloon target/current=%d/%d)",
			granted, allocAtSnap, balTarget, balCurrent)
	} else if allocAtSnap > yamlAlloc {
		logf("static mode: bumping initial allocatable from yaml=%d to snapshot allocatable=%d (balloon target/current=%d/%d)",
			yamlAlloc, allocAtSnap, balTarget, balCurrent)
	}
	// Record initialAlloc in hooks' in-memory state. No external write
	// here: cgroup memory.high is deferred to SettledRestore (Issue 4 —
	// PSI throttling during uffd-driven replay), and balloon already
	// reflects allocAtSnap from the snapshot (any correction needed
	// when initialAlloc != allocAtSnap also happens in SettledRestore,
	// after vm.resume).
	if hooks != nil {
		hooks.SetAllocatableNow(initialAlloc)
	}

	// Resolve disk reference (the snapshot's top disk layer). file:// is opened
	// directly; manifest:// goes through pkg/sandbox/disks.OpenManifestFetcher
	// (same path as cold-start manifest:// disks). Single-disk records it at
	// root.base; overlay at overlay.base.
	diskRef := snapCfg.Boot.Root.Base
	if !snapCfg.SingleDisk() {
		diskRef = snapCfg.Boot.Root.Overlay.Base
	}
	scheme, diskValue, ok := config.SchemeAndPath(diskRef)
	if !ok {
		return -1, fmt.Errorf("invalid disk base ref in snapshot.cfg: %s", diskRef)
	}
	if scheme == "file" && !filepath.IsAbs(diskValue) && opts.SnapshotPath != "" {
		// Resolve relative to snapshot file (local mode only).
		diskValue = filepath.Join(filepath.Dir(opts.SnapshotPath), diskValue)
	}
	if scheme == "manifest" && opts.Fetcher == nil {
		return -1, fmt.Errorf("restore: manifest:// disk in sandbox.cfg requires Fetcher")
	}
	logf("disk image: %s://%s", scheme, diskValue)

	// state.json restored verbatim (vCPU regs, virtio queue indices —
	// nothing path-dependent).
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), entries["state.json"], 0o644); err != nil {
		return -1, err
	}
	// config.json contains paths captured at snapshot time (uffd_socket,
	// vhost_socket, ch.sock api). Rewrite them to point at this run's
	// paths before handing to CH.
	vsockSock := filepath.Join(runDir, "vsock.sock")
	rewritten, err := rewriteConfigPaths(entries["config.json"], pathRewrite{
		UffdSocket: uffdSock,
		Blk0Sock:   blk0Sock,
		Blk1Sock:   blk1Sock,
		APISock:    chSock,
		VsockSock:  vsockSock,
	})
	if err != nil {
		return -1, fmt.Errorf("rewrite config.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), rewritten, 0o644); err != nil {
		return -1, err
	}

	// Memory capacity → memfd size (the memfd itself is owned by
	// sandbox.ServeAndWait). The uffd SnapshotSource is the only
	// restore-specific input to the shared uffd handler: Sparse (file)
	// or Manifest (chunk-granular via cache-ctl) instead of ZeroSource.
	capBytes, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}

	// Build the layered memory source: [self bundle] ++ from_refs (§3.5). A
	// non-resident page (hole) in an upper layer falls through to a lower
	// layer; a page hole in every layer (merged hole) → ZEROPAGE. Single layer
	// (no from_refs) degenerates to today's behaviour. The from_refs streams
	// live until the run exits (closed below); selfStream is closed at open.
	memLayers := []fetch.Stream{selfStream}
	for i, ref := range parsedSnap.FromRefs {
		s, err := openRefStream(ctx, ref, opts)
		if err != nil {
			return -1, fmt.Errorf("from_refs[%d] %q: %w", i, ref, err)
		}
		defer s.Close()
		memLayers = append(memLayers, s)
	}
	source, err := uffd.NewStreamSnapshotSource(ctx, fetch.NewLayered(memLayers...), capBytes)
	if err != nil {
		return -1, fmt.Errorf("snapshot source: %w", err)
	}
	logf("snapshot source: %d memory layer(s)", len(memLayers))

	// Open disk as base+diff: the read-only base is [top disk layer] ++
	// base_from_refs (§3.5) layered into one Stream, the new diff CoW'd on top.
	// baseReader.Close (deferred) closes the layered base and all its layers.
	// In single-disk mode this layered base IS blk0 (mounted rw directly); in
	// overlay mode it is blk1's lower (the erofs blk0 is opened separately below).
	diskLayers := []fetch.Stream{}
	topDisk, _, err := sandbox.OpenDiskStream(ctx, scheme+"://"+diskValue, opts.Fetcher)
	if err != nil {
		return -1, fmt.Errorf("open disk base: %w", err)
	}
	diskLayers = append(diskLayers, topDisk)
	for i, ref := range parentDiskChain {
		s, err := openRefStream(ctx, ref, opts)
		if err != nil {
			return -1, fmt.Errorf("base_from_refs[%d] %q: %w", i, ref, err)
		}
		diskLayers = append(diskLayers, s)
	}
	baseStream := fetch.NewLayered(diskLayers...)
	baseReader := vhost.NewStreamReader(ctx, baseStream, int64(baseStream.Size()))
	defer baseReader.Close()
	// Restore always builds a FRESH writable diff on top of the snapshot's
	// overlay (baseReader). Empty diff path → auto-default to the on-disk base
	// dir; an auto-defaulted diff is removed when this run ends. The base
	// provides the ext4, so a blank diff sized to it is mountable.
	diffURI, diffTemplate := snapCfg.Boot.Root.Diff, snapCfg.Boot.Root.DiffTemplate
	if !snapCfg.SingleDisk() {
		diffURI, diffTemplate = snapCfg.Boot.Root.Overlay.Diff, snapCfg.Boot.Root.Overlay.DiffTemplate
	}
	ownedDiff := diffURI == ""
	if ownedDiff {
		baseDir := sandbox.DefaultBaseDir(opts.BaseRoot, opts.SandboxID)
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return -1, fmt.Errorf("mkdir base dir %s: %w", baseDir, err)
		}
		diffURI = sandbox.DefaultDiffURI(opts.BaseRoot, opts.SandboxID)
		defer func() {
			_ = os.Remove(filepath.Join(baseDir, opts.SandboxID+".overlay.diff"))
			_ = os.Remove(baseDir)
		}()
	}
	_, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok {
		return -1, fmt.Errorf("bad root diff uri: %s", diffURI)
	}
	diffSize, err := snapCfg.DiffSizeBytes()
	if err != nil {
		return -1, err
	}
	createSize, err := sandbox.PrepareDiff(diffPath, diffTemplate, baseReader.Size(), diffSize)
	if err != nil {
		return -1, fmt.Errorf("prepare diff: %w", err)
	}
	cow, err := vhost.OpenBlockCOW(diffPath, baseReader, createSize)
	if err != nil {
		return -1, fmt.Errorf("open BlockCOW: %w", err)
	}
	defer cow.Close()

	// blk0. Overlay mode: the ro erofs base (from host yaml or snap.cfg);
	// file:// → local stream, manifest:// → fetch.Fetcher via cache-ctl.
	// Single-disk mode: blk0 IS the writable cow above (mounted rw directly),
	// so there is no separate erofs base — blk0Reader stays nil and blk0Path
	// labels the diff for stats.
	var blk0Reader vhost.BlockReader
	blk0Path := diffPath
	if !snapCfg.SingleDisk() {
		blk0Path = snapCfg.Boot.Root.Base
		if opts.HostCfg.Boot.Root.Base != "" {
			blk0Path = opts.HostCfg.Boot.Root.Base
		}
		r, _, err := sandbox.OpenBlockReader(ctx, blk0Path, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open blk0: %w", err)
		}
		blk0Reader = r
		defer blk0Reader.Close()
	}
	blk1Path := ""
	if !snapCfg.SingleDisk() {
		blk1Path = snapCfg.Boot.Root.Overlay.Diff
	}

	// Network: re-acquire the host side for this restore. tapfd mode re-runs
	// the handoff (docs/tapfd.md §6, idempotent) for a fresh queue fd, passed
	// to CH via --restore net_fds; tap-name mode lets CH reopen the named tap
	// from the restored config. The merged metadata also yields the NetworkSpec
	// the guest re-applies flush-and-replace (clone takes a fresh L3 identity;
	// the MAC stays the snapshot's, so the provider must use a stable per-port
	// MAC — see docs/tapfd.md §7).
	var tapFile, netnsFile *os.File
	var metaMAC, metaIP string
	var metaMTU int
	if snapCfg.Network.TapFD != nil {
		argv, err := snapCfg.Network.TapFD.ResolvedExec()
		if err != nil {
			return -1, err
		}
		f, nsf, meta, err := tapfd.Acquire(ctx, argv, snapCfg.Network.TapFD.TimeoutDuration())
		if err != nil {
			return -1, fmt.Errorf("tapfd handoff: %w", err)
		}
		tapFile = f
		defer tapFile.Close()
		netnsFile = nsf // non-nil only if the provider's tap is netns-isolated
		if netnsFile != nil {
			defer netnsFile.Close()
		}
		metaMAC, metaIP, metaMTU = meta.MAC, meta.IP, meta.MTU
		logf("tapfd: received tap fd for restore (mac=%s ip=%s mtu=%d netns=%t)", meta.MAC, meta.IP, meta.MTU, netnsFile != nil)
	}
	netMAC, netSpec := snapCfg.Network.Effective(metaMAC, metaIP, metaMTU)

	// The shared back-half (memfd, uffd va_report handler, vhost-blk
	// backends, the launch server — incl. the guest→host mem_report /
	// app_exited channel that was missing on the restore path — pinger,
	// ctl.sock, signal escalation, stats) lives in sandbox.ServeAndWait.
	// Restore supplies: a snapshot uffd Source (not ZeroSource), a
	// placeholder launch spec (the guest does NOT re-hello after a
	// restore, so WireLaunchMUX=false — the stdio MUX is re-established
	// by PostSpawn over the reverse channel), and a settle protocol of
	// waitAPI → /vm.resume → restore{epoch} → SettledRestore.
	return sandbox.ServeAndWait(sandbox.VMParams{
		Ctx:                ctx,
		SandboxID:          opts.SandboxID,
		RunDir:             runDir,
		Logf:               logf,
		StdioMode:          opts.StdioMode,
		PingFatalThreshold: opts.PingFatalThreshold,
		StartUnixNs:        startUnixNs,
		StatsJSONPath:      opts.StatsJSONPath,
		StatsInterval:      opts.StatsInterval,
		VAReportDeadline:   opts.HostCfg.VAReportDeadline(),
		PingTimeout:        opts.HostCfg.PingDeadline(),
		AppNotifyDeadline:  opts.HostCfg.AppNotifyDeadline(),

		CapBytes:   int64(capBytes),
		UffdSource: source,
		SingleDisk: snapCfg.SingleDisk(),
		Blk0Reader: blk0Reader, // nil in single-disk (blk0 is the Cow)
		Blk0Label:  "blk0",
		Blk0Path:   blk0Path,
		Cow:        cow,
		Blk1Label:  "blk1",
		Blk1Path:   blk1Path,

		LaunchSpec:    &proto.LaunchSpec{},
		WireLaunchMUX: false,
		Balloon:       balloonCtl,
		Hooks:         hooks,

		TapFile:   tapFile, // nil in tap-name mode; CH inherits it at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:     &snapCfg,
		ManifestCfg: opts.ManifestCfg,
		DiffPath:    diffPath,
		OwnedDiff:   ownedDiff,
		Forwards:    opts.Forwards,
		Cgroup:      cg,

		BuildCmd: func(e sandbox.CmdEnv) (*exec.Cmd, func(), error) {
			// CH 51 `--restore source_url=file://<dir>` replaces
			// --kernel/--vsock; --console/--serial are restored from the
			// snapshot bundle (taken with `--console tty --serial off`),
			// so we don't repeat them. consoleArg is unused here.
			cmd := exec.CommandContext(ctx, opts.CHBinary)
			_, cleanup, err := opts.StdioMode.SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, fmt.Errorf("stdio: %w", err)
			}
			restoreArg := "source_url=file://" + stateDir
			if e.TapFDNum > 0 {
				// CH can't serialize fds, so the snapshot's net fd is dead;
				// re-bind the fresh tap queue fd (CH fd 4) to the restored net
				// device named _net0 at cold boot via net_fds.
				// net_fds is a CH Tuple<String,Vec<u64>>: the whole value is
				// bracket-wrapped, each entry is <net-id>@<fd-list>. Single
				// net _net0 with one fd → [_net0@[N]].
				restoreArg += fmt.Sprintf(",net_fds=[_net0@[%d]]", e.TapFDNum)
			}
			cmd.Args = append(cmd.Args, "--api-socket", e.CHSock, "--restore", restoreArg)
			logf("spawning %s --api-socket %s --restore %s", opts.CHBinary, e.CHSock, restoreArg)
			return cmd, cleanup, nil
		},

		// Restore settle (docs/sandbox.md §7 T14-T15): wait for CH's
		// API, /vm.resume to release the vCPUs from the snapshot point,
		// then notify the guest (restore{epoch=1}) and turn that
		// reverse-channel conn into the stdio MUX. Synchronous — a
		// non-nil return aborts the run (ServeAndWait kills CH); we
		// don't hand back a sandbox whose guest agent is unreachable.
		PostSpawn: func(pc sandbox.PostSpawnCtx) error {
			if err := chapi.WaitReady(ctx, pc.CHSock, opts.HostCfg.APIReadyDeadline()); err != nil {
				return fmt.Errorf("ch api not ready: %w", err)
			}
			if err := (chapi.Client{Sock: pc.CHSock, RespDeadline: opts.HostCfg.CHApiDeadline()}).Resume(); err != nil {
				return fmt.Errorf("vm.resume: %w", err)
			}
			pc.Logf("VM resumed, vCPU running")

			tRestore := time.Now()
			// 0 = no forced timeout: DialRaw needs a finite value, so fall back
			// to noForcedTimeout (effective-infinity; cancellation still flows
			// via ctx → CH teardown closing the vsock conn).
			restoreDeadline := opts.HostCfg.RestoreDeadline()
			if restoreDeadline <= 0 {
				restoreDeadline = config.NoForcedTimeout
			}
			muxConn, muxSpec, err := guestlink.OpenMUXViaRestore(pc.Pinger.Client, 1, netSpec, snapCfg.ProtoFiles(), restoreDeadline)
			if err != nil {
				return fmt.Errorf("notify restore: %w (guest agent unreachable)", err)
			}
			if err := pc.EstablishMUX(muxConn, muxSpec); err != nil {
				return fmt.Errorf("stdio MUX bridge: %w", err)
			}
			pc.Logf("restore notify acked in %dµs (stdio MUX re-established: tty=%v); starting ping ticker",
				time.Since(tRestore).Microseconds(), muxSpec.TTY)
			pc.Pinger.Start(pc.Ctx)
			// Balloon reconcile: idempotent — if initialAlloc ==
			// allocAtSnap, target matches what CH loaded from state.json.
			if pc.Balloon != nil {
				if err := pc.Balloon.Start(pc.Ctx); err != nil {
					pc.Logf("balloon: start: %v", err)
				}
			}
			// Restore-path settled trigger (docs/sandbox.md §10.1):
			// restore_ack is the controller's equivalent of cold-start
			// hello. Writes memory.high (deferred from JoinCgroup —
			// Issue 4) using allocatable_now; corrects balloon only when
			// initialAlloc != allocAtSnap.
			if pc.Hooks != nil {
				if err := pc.Hooks.SettledRestore(allocAtSnap); err != nil {
					pc.Logf("settled-restore: %v (continuing)", err)
				}
				if pc.Hooks.Enabled() {
					pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
					pc.Hooks.StartSensor(pc.Ctx, 64<<20)
				}
			}
			return nil
		},
	})
}

// openRefStream resolves a from_refs / base_from_refs entry (§3.5) into a
// fetch.Stream. file:// refs are content-addressed basenames located relative
// to the snapshot bundle dir (local mode); manifest:// refs go through the
// fetcher. Shared by the memory and disk layered chains.
func openRefStream(ctx context.Context, ref string, opts Options) (fetch.Stream, error) {
	scheme, value, ok := config.SchemeAndPath(ref)
	if !ok {
		return nil, fmt.Errorf("invalid ref %q", ref)
	}
	if scheme == "file" && !filepath.IsAbs(value) && opts.SnapshotPath != "" {
		value = filepath.Join(filepath.Dir(opts.SnapshotPath), value)
	}
	s, _, err := sandbox.OpenDiskStream(ctx, scheme+"://"+value, opts.Fetcher)
	return s, err
}

// fileSnapshotRef returns the content-addressed ref for a file-mode snapshot
// bundle: it follows a <sid>.snapshot symlink to the real <sha256>.snapshot
// and returns file://<basename>. Recorded into a child snapshot's from_refs.
func fileSnapshotRef(path string) string {
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	return "file://" + filepath.Base(real)
}
