package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalCold = `
resources:
  capacity:
    cpu: 2
    memory: 2GiB
  allocatable:
    cpu: 1.5
    memory: 1GiB
network:
  tap: tap0
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.erofs
  cmdline: "console=hvc0 ip=169.254.1.1::169.254.1.0:255.255.255.254:test:eth0:off"
  root:
    base: file:///container.erofs
    overlay:
      diff: file:///run/sb/diff.ext4
      size: 1GiB
launch:
  exec: /usr/bin/echo
  args: ["hello", "world"]
`

func TestLoad_ValidCold(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Capacity.CPU != 2 {
		t.Errorf("Capacity.CPU = %d", cfg.Resources.Capacity.CPU)
	}
	if cfg.Launch.Exec != "/usr/bin/echo" {
		t.Errorf("Launch.Exec = %q", cfg.Launch.Exec)
	}
	if len(cfg.Launch.Args) != 2 || cfg.Launch.Args[0] != "hello" {
		t.Errorf("Launch.Args = %v", cfg.Launch.Args)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Errorf("ValidateCold: %v", err)
	}
}

func TestLoad_DefaultsAllocFromCapacity(t *testing.T) {
	cfg, err := Load(writeYAML(t, `
resources:
  capacity:
    cpu: 2
    memory: 4GiB
network:
  tap: tap0
boot:
  kernel: file:///vmlinux
  runtime: file:///runtime.erofs
  root:
    base: file:///c.erofs
    overlay:
      diff: file:///d.ext4
launch:
  exec: /bin/true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Allocatable.CPU != 2 {
		t.Errorf("Allocatable.CPU should default to capacity, got %v", cfg.Resources.Allocatable.CPU)
	}
	if cfg.Resources.Allocatable.Memory != "4GiB" {
		t.Errorf("Allocatable.Memory should default to capacity, got %q", cfg.Resources.Allocatable.Memory)
	}
}

func TestMemoryParsing(t *testing.T) {
	cfg, _ := Load(writeYAML(t, minimalCold))
	got, err := cfg.CapacityMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got != 2<<30 {
		t.Errorf("CapacityMemoryBytes = %d, want %d", got, 2<<30)
	}
	got, err = cfg.AllocatableMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1<<30 {
		t.Errorf("AllocatableMemoryBytes = %d, want %d", got, 1<<30)
	}
}

func TestOverlaySize_Defaults(t *testing.T) {
	cfg, _ := Load(writeYAML(t, `
resources:
  capacity: { cpu: 1, memory: 256MiB }
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay:
      diff: file:///d
launch: { exec: /bin/true }
`))
	got, err := cfg.OverlaySize()
	if err != nil {
		t.Fatal(err)
	}
	if got != 10<<30 {
		t.Errorf("default overlay size = %d, want 10GiB", got)
	}
}

func TestValidateCold_MissingFields(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *SandboxConfig)
		wantSubst string
	}{
		{"no kernel", func(c *SandboxConfig) { c.Boot.Kernel = "" }, "boot.kernel"},
		{"manifest kernel", func(c *SandboxConfig) { c.Boot.Kernel = "manifest://abc" }, "must be file"},
		{"no runtime", func(c *SandboxConfig) { c.Boot.Runtime = "" }, "boot.runtime"},
		// no exec is now allowed (image config can supply it; MergeLaunch fails late if neither does)
		{"no root.base", func(c *SandboxConfig) { c.Boot.Root.Base = "" }, "boot.root.base"},
		{"no overlay.diff", func(c *SandboxConfig) { c.Boot.Root.Overlay.Diff = "" }, "boot.root.overlay.diff"},
		{"manifest overlay.diff", func(c *SandboxConfig) { c.Boot.Root.Overlay.Diff = "manifest://abc" }, "must be file"},
		{"no tap", func(c *SandboxConfig) { c.Network.TAP = "" }, "network.tap"},
		{"alloc mem > capacity", func(c *SandboxConfig) {
			c.Resources.Allocatable.Memory = "16GiB"
		}, "allocatable.memory must be ≤"},
		{"alloc cpu > capacity", func(c *SandboxConfig) {
			c.Resources.Allocatable.CPU = 99
		}, "allocatable.cpu must be ≤"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, minimalCold))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.ValidateCold()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubst)
			}
			if !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubst)
			}
		})
	}
}

func TestSchemeAndPath(t *testing.T) {
	cases := []struct {
		uri    string
		scheme string
		val    string
		ok     bool
	}{
		{"file:///foo/bar", "file", "/foo/bar", true},
		{"manifest://abcd", "manifest", "abcd", true},
		{"https://example.com", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		s, v, ok := SchemeAndPath(c.uri)
		if s != c.scheme || v != c.val || ok != c.ok {
			t.Errorf("SchemeAndPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.uri, s, v, ok, c.scheme, c.val, c.ok)
		}
	}
}
