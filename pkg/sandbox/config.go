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
	"strings"

	"github.com/fullof-work/mass-sandbox/pkg/config"
	"gopkg.in/yaml.v3"
)

// SandboxConfig is the schema parsed from sandbox.yaml.
//
// The file co-exists with the standard accelerator.yaml — manifest /
// store / crypto / cache fields live in accelerator.yaml (loaded
// separately via pkg/config). Sandbox.yaml carries only the per-sandbox
// resource / network / boot / launch fields.
type SandboxConfig struct {
	Resources ResourcesConfig `yaml:"resources"`
	Network   NetworkConfig   `yaml:"network"`
	Boot      BootConfig      `yaml:"boot"`
	Launch    LaunchConfig    `yaml:"launch"`
}

// ResourcesConfig follows Kubernetes-style capacity / allocatable split:
// capacity is what the guest sees, allocatable is what the host actually
// guarantees (≤ capacity). The difference is reclaimed via virtio-balloon
// and cgroup limits.
type ResourcesConfig struct {
	Capacity    CapacityConfig    `yaml:"capacity"`
	Allocatable AllocatableConfig `yaml:"allocatable"`
}

type CapacityConfig struct {
	CPU    int    `yaml:"cpu"`    // vCPUs declared to guest
	Memory string `yaml:"memory"` // human-readable size, e.g. "8GiB"
}

type AllocatableConfig struct {
	// CPU in fractional cores; cgroup cpu.max = (CPU * 100000us per 100000us period).
	CPU float64 `yaml:"cpu"`
	// Memory cgroup memory.max as a human-readable size, e.g. "128MiB".
	// Balloon inflates to (capacity.memory - allocatable.memory) at boot.
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

	if c.Boot.Kernel == "" {
		return errors.New("boot.kernel is required for cold start")
	}
	if !strings.HasPrefix(c.Boot.Kernel, "file://") {
		return errors.New("boot.kernel must be file://")
	}
	if c.Boot.Runtime == "" {
		return errors.New("boot.runtime is required")
	}
	if !strings.HasPrefix(c.Boot.Runtime, "file://") {
		return errors.New("boot.runtime must be file://")
	}

	if c.Boot.Root.Base == "" {
		return errors.New("boot.root.base is required")
	}
	if c.Boot.Root.Overlay.Diff == "" {
		return errors.New("boot.root.overlay.diff is required")
	}
	if !strings.HasPrefix(c.Boot.Root.Overlay.Diff, "file://") {
		return errors.New("boot.root.overlay.diff must be file://")
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

// AcceleratorConfig is the shared manifest/store/cache/crypto config
// loaded from accelerator.yaml. Sandbox-ctl reuses the same loader as
// manifest-ctl.
type AcceleratorConfig = config.Config

// LoadAccelerator returns the accelerator config from the standard
// search path or the explicit --config flag.
func LoadAccelerator(explicitPath string) (*AcceleratorConfig, error) {
	if explicitPath != "" {
		return config.LoadFile(explicitPath)
	}
	return config.FindAndLoad()
}
