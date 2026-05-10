// Package sandbox holds the host-side control plane for one sandbox VM.
// It builds the cloud-hypervisor command line, spawns the VMM, manages
// vhost-user-blk backends, and handles cgroup + lifecycle.
//
// One sandbox-ctl process owns one sandbox. Restart = relaunch.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fullof-work/mass-sandbox/pkg/config"
	"gopkg.in/yaml.v3"
)

// SandboxConfig is the schema parsed from sandbox.yaml.
//
// The file co-exists with the manifest config YAML — manifest /
// store / crypto / cache fields live there (loaded separately via
// pkg/config). Sandbox.yaml carries only the per-sandbox
// resource / network / boot / launch fields.
type SandboxConfig struct {
	Resources ResourcesConfig `yaml:"resources"`
	Network   NetworkConfig   `yaml:"network"`
	Boot      BootConfig      `yaml:"boot"`
	Launch    LaunchConfig    `yaml:"launch"`

	// SnapshotRefs is computed at sandbox boot (lifecycle.go fills it
	// before snapshot is possible) and not part of the YAML schema.
	// Exposed publicly so snapshot.cfg builder + applyrules can read.
	SnapshotRefs SnapshotRefs `yaml:"-"`
}

// SnapshotRefs holds the precomputed `file://<basename>@sha256:<digest>`
// (or `manifest://<key>`) refs for boot.runtime and boot.root.base, used
// when synthesising snapshot.cfg.
type SnapshotRefs struct {
	RuntimeRef string // file://<basename>@sha256:<digest>
	BaseRef    string // file://<basename>@sha256:<digest> or manifest://<key>
}

// ResourcesConfig follows Kubernetes-style capacity / allocatable split:
// capacity is what the guest sees, allocatable is what the host actually
// guarantees (≤ capacity). The difference is reclaimed via virtio-balloon
// and cgroup limits.
//
// See docs/sandbox.md §4.1 for the three deployment modes driven by
// Control.CgroupPath / Control.Controller presence.
type ResourcesConfig struct {
	Capacity    CapacityConfig    `yaml:"capacity"`
	Allocatable AllocatableConfig `yaml:"allocatable"`

	// Control gates cgroup management and dynamic resource control.
	// Empty CgroupPath = no-cgroup mode; CgroupPath set + Controller empty
	// = static-cgroup mode; both set = dynamic mode (controller-managed).
	Control ControlConfig `yaml:"control,omitempty"`

	// Overhead, WatermarkHigh, StartupBurst use pointers so we can
	// distinguish "not set" from "set to zero". They are only valid when
	// the gating field is set (see ValidateCold).
	Overhead      *OverheadConfig      `yaml:"overhead,omitempty"`
	WatermarkHigh *WatermarkHighConfig `yaml:"watermark_high,omitempty"`
	StartupBurst  *StartupBurstConfig  `yaml:"startup_burst,omitempty"`
}

type CapacityConfig struct {
	CPU    int    `yaml:"cpu"`    // vCPUs declared to guest; written to --cpus boot=N
	Memory string `yaml:"memory"` // human-readable size, e.g. "8GiB"
}

type AllocatableConfig struct {
	// CPU in fractional cores. With cgroup_path set, drives cpu.weight =
	// clamp(round(CPU * 100), 1, 10000). Without cgroup_path, must equal
	// capacity.cpu (no fractional CPU without cgroup).
	CPU float64 `yaml:"cpu"`
	// Memory drives balloon initial size = capacity.memory - this value.
	// Independent of cgroup; balloon is a CH-side mechanism.
	Memory string `yaml:"memory"`
	// DeflateOnOOM toggles CH --balloon ,deflate_on_oom=on. Pointer so
	// nil = use default (true). Only applies when balloon is configured
	// (allocatable.memory < capacity.memory); otherwise ignored with warn.
	DeflateOnOOM *bool `yaml:"deflate_on_oom,omitempty"`
}

// ControlConfig gates cgroup management and dynamic resource control.
// Both fields are optional; their presence selects deployment mode.
type ControlConfig struct {
	// CgroupPath is the absolute path of an existing cgroup directory.
	// sandbox-ctl joins this cgroup (writes limits + adds self PID); it
	// never creates the directory and never rmdir on exit. Empty disables
	// all cgroup operations (no-cgroup mode).
	CgroupPath string `yaml:"cgroup_path,omitempty"`
	// Controller is the UDS path of a sandbox-resource-control protocol
	// endpoint. Non-empty enables dynamic mode (M2+). Requires CgroupPath.
	Controller string `yaml:"controller,omitempty"`
}

// OverheadConfig adjusts cgroup memory.max above capacity.memory to give
// CH process internal allocations + sandbox-ctl Go runtime headroom.
// Without this overhead, memory.max = capacity.memory and CH may be
// cgroup-OOM-killed under normal operation.
type OverheadConfig struct {
	Memory string `yaml:"memory"` // memory.max = capacity.memory + this
}

// WatermarkHighConfig sets cgroup memory.high — the PSI throttling
// threshold below memory.max. Default is allocatable.memory * 0.875.
type WatermarkHighConfig struct {
	Memory string `yaml:"memory"`
}

// StartupBurstConfig sets the elevated initial allocatable_now during
// the startup phase (before launch hello / restored). Drops to
// allocatable.memory after settled. Only meaningful in dynamic mode
// (controller set); admission reserves this amount up-front.
type StartupBurstConfig struct {
	Memory string `yaml:"memory"`
}

// NetworkConfig declares both the host-side TAP attachment and the
// guest-side IP layer config. The IP/Gateway/Hostname/Interface fields
// are propagated to sandbox-init via the launch protocol; sandbox-init
// applies them via netlink before forking the user app. Replaces the
// kernel's `ip=...` cmdline + CONFIG_IP_PNP path.
type NetworkConfig struct {
	TAP       string `yaml:"tap"`                 // pre-existing host TAP name (sandbox-ctl attaches, doesn't create)
	Interface string `yaml:"interface,omitempty"` // guest iface name; defaults to "eth0"
	IP        string `yaml:"ip,omitempty"`        // CIDR (IPv4 or IPv6), e.g. "169.254.1.1/31". Empty → no IP config.
	Gateway   string `yaml:"gateway,omitempty"`   // default route next-hop; empty → no default route
	Hostname  string `yaml:"hostname,omitempty"`  // guest hostname (sethostname)
}

// BootConfig is everything the kernel needs to start: kernel image,
// extra cmdline, sandbox-runtime image, and the rootfs (base + COW
// overlay). The init parameters and rootfs mount options are auto-
// injected by sandbox-ctl; user only provides extras like console=
// and ip= via cmdline.
type BootConfig struct {
	Kernel  string     `yaml:"kernel"`  // file:// only (cold-start)
	Cmdline string     `yaml:"cmdline"` // user extras; merged with auto-injected base
	Runtime string     `yaml:"runtime"` // file:// sandbox-runtime.erofs path
	Root    RootConfig `yaml:"root"`
}

type RootConfig struct {
	// Base is the read-only container image (flattened erofs).
	// Auto-mounted as disk0 (vhost-user-blk readonly).
	Base    string        `yaml:"base"`
	// Overlay is the writable upper layer (ext4 base + sparse diff).
	// Auto-mounted as disk1 (vhost-user-blk read-write).
	Overlay OverlayConfig `yaml:"overlay"`
}

type OverlayConfig struct {
	// Base is an optional read-only ext4 layer (e.g. a snapshot's prior dirty
	// state). May be file:// or manifest://. Empty for fresh sandboxes.
	Base string `yaml:"base"`
	// Diff is the local sparse ext4 file collecting writes since boot.
	// file:// only. Created sparse if missing.
	Diff string `yaml:"diff"`
	// Size is the visible block-device size. Optional; defaults to
	// max(base size if any, diff existing size) or 10 GiB if neither.
	Size string `yaml:"size"`
}

// LaunchConfig overrides the container's default launch (which lives
// in the flattened image's appended config.json). For v1 the override
// is mandatory — auto-extraction of image config is future work.
type LaunchConfig struct {
	Exec    string            `yaml:"exec"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Workdir string            `yaml:"workdir"`
	Restart string            `yaml:"restart"` // never|on-failure|always
}

// Load reads sandbox.yaml at the given path and applies defaults.
func Load(path string) (*SandboxConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %s: %w", path, err)
	}
	var cfg SandboxConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("sandbox: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *SandboxConfig) applyDefaults() {
	if c.Resources.Capacity.CPU == 0 {
		c.Resources.Capacity.CPU = 1
	}
	if c.Resources.Allocatable.CPU == 0 {
		c.Resources.Allocatable.CPU = float64(c.Resources.Capacity.CPU)
	}
	if c.Resources.Allocatable.Memory == "" {
		c.Resources.Allocatable.Memory = c.Resources.Capacity.Memory
	}
	if c.Launch.Restart == "" {
		c.Launch.Restart = "never"
	}
	if c.Launch.Workdir == "" {
		c.Launch.Workdir = "/"
	}
	if c.Network.Interface == "" {
		c.Network.Interface = "eth0"
	}
}

// CapacityMemoryBytes returns the parsed capacity memory in bytes.
func (c *SandboxConfig) CapacityMemoryBytes() (uint64, error) {
	if c.Resources.Capacity.Memory == "" {
		return 0, errors.New("resources.capacity.memory is required")
	}
	return config.ParseSize(c.Resources.Capacity.Memory)
}

// AllocatableMemoryBytes returns the parsed allocatable memory in bytes.
func (c *SandboxConfig) AllocatableMemoryBytes() (uint64, error) {
	if c.Resources.Allocatable.Memory == "" {
		return c.CapacityMemoryBytes()
	}
	return config.ParseSize(c.Resources.Allocatable.Memory)
}

// DeflateOnOOM returns whether CH --balloon should carry deflate_on_oom=on.
// Default is true; only false if user explicitly set it false.
func (c *SandboxConfig) DeflateOnOOM() bool {
	if c.Resources.Allocatable.DeflateOnOOM == nil {
		return true
	}
	return *c.Resources.Allocatable.DeflateOnOOM
}

// CgroupPath returns the configured cgroup path. Empty means no-cgroup mode
// (no cgroup operations).
func (c *SandboxConfig) CgroupPath() string {
	return c.Resources.Control.CgroupPath
}

// OverheadMemoryBytes returns the cgroup memory.max overhead. Only
// meaningful when CgroupPath is set; default is 32 MiB to give CH +
// sandbox-ctl headroom and avoid cgroup OOM under normal operation.
// Returns 0 when CgroupPath is empty (caller should not use).
func (c *SandboxConfig) OverheadMemoryBytes() (uint64, error) {
	if c.Resources.Control.CgroupPath == "" {
		return 0, nil
	}
	if c.Resources.Overhead == nil {
		return 32 << 20, nil
	}
	return config.ParseSize(c.Resources.Overhead.Memory)
}

// WatermarkHighBytes returns the cgroup memory.high initial value.
// Only meaningful when CgroupPath is set; default = allocatable.memory * 0.875.
// Returns 0 when CgroupPath is empty (caller should not use).
func (c *SandboxConfig) WatermarkHighBytes() (uint64, error) {
	if c.Resources.Control.CgroupPath == "" {
		return 0, nil
	}
	if c.Resources.WatermarkHigh == nil {
		alloc, err := c.AllocatableMemoryBytes()
		if err != nil {
			return 0, err
		}
		return uint64(float64(alloc) * 0.875), nil
	}
	return config.ParseSize(c.Resources.WatermarkHigh.Memory)
}

// StartupBurstBytes returns the elevated startup-phase allocatable_now.
// Only meaningful when Controller is set; default = allocatable.memory.
// Returns allocatable when Controller is empty (caller treats startup
// as if no burst).
func (c *SandboxConfig) StartupBurstBytes() (uint64, error) {
	if c.Resources.Control.Controller == "" {
		return c.AllocatableMemoryBytes()
	}
	if c.Resources.StartupBurst == nil {
		return c.AllocatableMemoryBytes()
	}
	return config.ParseSize(c.Resources.StartupBurst.Memory)
}

// CPUWeight maps allocatable.cpu to a cgroup v2 cpu.weight value in
// [1, 10000]. Mapping is allocatable.cpu * 100 (so 1 core = 100 = kernel
// default). Used only when CgroupPath is set.
func (c *SandboxConfig) CPUWeight() uint64 {
	w := int64(c.Resources.Allocatable.CPU * 100)
	if w < 1 {
		w = 1
	}
	if w > 10000 {
		w = 10000
	}
	return uint64(w)
}

// OverlaySize returns the resolved visible block-device size for the
// overlay (disk1) in bytes. If user didn't set Size, it defaults to
// 10 GiB. Caller is responsible for cross-checking against existing
// diff/base file sizes.
func (c *SandboxConfig) OverlaySize() (int64, error) {
	if c.Boot.Root.Overlay.Size == "" {
		return 10 << 30, nil
	}
	v, err := config.ParseSize(c.Boot.Root.Overlay.Size)
	if err != nil {
		return 0, fmt.Errorf("boot.root.overlay.size: %w", err)
	}
	return int64(v), nil
}

// ValidateCold checks invariants required for the cold-start path.
//
// Resource-control gating rules (see docs/sandbox.md §13):
//   - Controller requires CgroupPath
//   - Overhead / WatermarkHigh require CgroupPath
//   - StartupBurst requires Controller
//   - allocatable.cpu == capacity.cpu when CgroupPath is empty (no
//     fractional CPU without cgroup)
//   - CgroupPath must exist on the host filesystem
//   - StartupBurst.memory ∈ [allocatable.memory, capacity.memory]
//   - WatermarkHigh.memory ∈ (0, allocatable.memory]
func (c *SandboxConfig) ValidateCold() error {
	if c.Resources.Capacity.CPU <= 0 {
		return errors.New("resources.capacity.cpu must be > 0")
	}
	capMem, err := c.CapacityMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.capacity.memory: %w", err)
	}
	allocMem, err := c.AllocatableMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.allocatable.memory: %w", err)
	}
	if allocMem > capMem {
		return errors.New("resources.allocatable.memory must be ≤ capacity.memory")
	}
	if c.Resources.Allocatable.CPU > float64(c.Resources.Capacity.CPU) {
		return errors.New("resources.allocatable.cpu must be ≤ capacity.cpu")
	}
	if c.Resources.Allocatable.CPU <= 0 {
		return errors.New("resources.allocatable.cpu must be > 0")
	}

	// Resource-control gating
	cgroupSet := c.Resources.Control.CgroupPath != ""
	controllerSet := c.Resources.Control.Controller != ""

	if controllerSet && !cgroupSet {
		return errors.New("resources.control.controller requires resources.control.cgroup_path")
	}
	if !cgroupSet && c.Resources.Overhead != nil {
		return errors.New("resources.overhead requires resources.control.cgroup_path")
	}
	if !cgroupSet && c.Resources.WatermarkHigh != nil {
		return errors.New("resources.watermark_high requires resources.control.cgroup_path")
	}
	if !controllerSet && c.Resources.StartupBurst != nil {
		return errors.New("resources.startup_burst requires resources.control.controller")
	}
	if !cgroupSet && c.Resources.Allocatable.CPU != float64(c.Resources.Capacity.CPU) {
		return fmt.Errorf("resources.allocatable.cpu must equal capacity.cpu (%d) when cgroup_path is not set; got %g (fractional cpu requires cgroup_path)",
			c.Resources.Capacity.CPU, c.Resources.Allocatable.CPU)
	}

	// CgroupPath existence
	if cgroupSet {
		st, err := os.Stat(c.Resources.Control.CgroupPath)
		if err != nil {
			return fmt.Errorf("resources.control.cgroup_path %q does not exist: %w", c.Resources.Control.CgroupPath, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("resources.control.cgroup_path %q is not a directory", c.Resources.Control.CgroupPath)
		}
	}

	// Overhead value
	if c.Resources.Overhead != nil {
		if _, err := config.ParseSize(c.Resources.Overhead.Memory); err != nil {
			return fmt.Errorf("resources.overhead.memory: %w", err)
		}
	}

	// WatermarkHigh ∈ (0, allocatable.memory]
	if c.Resources.WatermarkHigh != nil {
		wm, err := config.ParseSize(c.Resources.WatermarkHigh.Memory)
		if err != nil {
			return fmt.Errorf("resources.watermark_high.memory: %w", err)
		}
		if wm == 0 {
			return errors.New("resources.watermark_high.memory must be > 0")
		}
		if wm > allocMem {
			return fmt.Errorf("resources.watermark_high.memory (%d) must be ≤ allocatable.memory (%d)", wm, allocMem)
		}
	}

	// StartupBurst ∈ [allocatable.memory, capacity.memory]
	if c.Resources.StartupBurst != nil {
		sb, err := config.ParseSize(c.Resources.StartupBurst.Memory)
		if err != nil {
			return fmt.Errorf("resources.startup_burst.memory: %w", err)
		}
		if sb < allocMem {
			return fmt.Errorf("resources.startup_burst.memory (%d) must be ≥ allocatable.memory (%d)", sb, allocMem)
		}
		if sb > capMem {
			return fmt.Errorf("resources.startup_burst.memory (%d) must be ≤ capacity.memory (%d)", sb, capMem)
		}
	}

	if c.Boot.Kernel == "" {
		return errors.New("boot.kernel is required for cold start")
	}
	if err := requireFileAbs("boot.kernel", c.Boot.Kernel); err != nil {
		return err
	}
	if c.Boot.Runtime == "" {
		return errors.New("boot.runtime is required")
	}
	if err := requireFileAbs("boot.runtime", c.Boot.Runtime); err != nil {
		return err
	}

	if c.Boot.Root.Base == "" {
		return errors.New("boot.root.base is required")
	}
	if err := requireAbsIfFile("boot.root.base", c.Boot.Root.Base); err != nil {
		return err
	}
	if c.Boot.Root.Overlay.Base != "" {
		if err := requireAbsIfFile("boot.root.overlay.base", c.Boot.Root.Overlay.Base); err != nil {
			return err
		}
	}
	if c.Boot.Root.Overlay.Diff == "" {
		return errors.New("boot.root.overlay.diff is required")
	}
	if err := requireFileAbs("boot.root.overlay.diff", c.Boot.Root.Overlay.Diff); err != nil {
		return err
	}
	if _, err := c.OverlaySize(); err != nil {
		return err
	}

	if c.Network.TAP == "" {
		return errors.New("network.tap is required")
	}

	// launch.exec is no longer required: if the rootfs erofs has an
	// appended config.json with Entrypoint or Cmd, those are used as
	// defaults. MergeLaunch fails late with a clear error if neither
	// the override nor the image provides an executable.

	return nil
}

// requireFileAbs enforces that uri starts with file:// and the path is
// absolute. Used for fields that only accept file:// (kernel, runtime,
// overlay.diff).
func requireFileAbs(field, uri string) error {
	if !strings.HasPrefix(uri, "file://") {
		return fmt.Errorf("%s must be file://", field)
	}
	p := strings.TrimPrefix(uri, "file://")
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s file:// must be absolute (got %q)", field, uri)
	}
	return nil
}

// requireAbsIfFile permits manifest:// and otherwise enforces an
// absolute file:// path. Used for fields that accept both schemes
// (root.base, overlay.base).
func requireAbsIfFile(field, uri string) error {
	if strings.HasPrefix(uri, "manifest://") {
		return nil
	}
	return requireFileAbs(field, uri)
}

// SchemeAndPath splits a URI like "file:///path" or "manifest://hexkey"
// into ("file", "/path") or ("manifest", "hexkey").
func SchemeAndPath(uri string) (scheme, value string, ok bool) {
	const filePrefix = "file://"
	const manifestPrefix = "manifest://"
	switch {
	case strings.HasPrefix(uri, filePrefix):
		return "file", strings.TrimPrefix(uri, filePrefix), true
	case strings.HasPrefix(uri, manifestPrefix):
		return "manifest", strings.TrimPrefix(uri, manifestPrefix), true
	}
	return "", "", false
}

// ManifestConfig is the shared manifest/store/cache/crypto config the
// sandbox-ctl uses for any `manifest://` resource (boot blk0 base,
// snapshot bundle, snapshot --upload). Same shape as manifest-ctl's
// config so the same YAML drives both.
type ManifestConfig = config.Config

// ManifestConfigEnv is the process-environment variable consulted as
// a fallback when --manifest-config is not passed on the CLI.
const ManifestConfigEnv = "MANIFEST_CONFIG"

// LoadManifestConfig returns the manifest config, choosing the file
// path in order: flagPath, then $MANIFEST_CONFIG. Returns
// (nil, config.ErrConfigNotProvided) when neither is set — callers
// running with file://-only resources may treat that as a soft skip.
func LoadManifestConfig(flagPath string) (*ManifestConfig, error) {
	return config.LoadFromFlagOrEnv(flagPath, ManifestConfigEnv)
}
