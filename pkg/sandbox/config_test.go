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
    cpu: 2
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
		{"no network source", func(c *SandboxConfig) { c.Network.TAP = "" }, "exactly one of"},
		{"both tap and tapfd", func(c *SandboxConfig) {
			c.Network.TapFD = &TapFDConfig{Exec: []string{"helper"}}
		}, "exactly one of"},
		{"tapfd without exec", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{}
		}, "tapfd.exec is required"},
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

func TestValidateCold_ResourceControl(t *testing.T) {
	// Helper: write a minimal config and apply mutator before validating.
	run := func(t *testing.T, mutate func(c *SandboxConfig), wantErrSubstr string) {
		t.Helper()
		cfg, err := Load(writeYAML(t, minimalCold))
		if err != nil {
			t.Fatal(err)
		}
		mutate(cfg)
		err = cfg.ValidateCold()
		if wantErrSubstr == "" {
			if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("expected error containing %q, got nil", wantErrSubstr)
		}
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("error %q does not contain %q", err.Error(), wantErrSubstr)
		}
	}

	t.Run("controller without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.Controller = "/run/x.sock"
		}, "controller requires resources.control.cgroup_path")
	})

	t.Run("overhead without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Overhead = &OverheadConfig{Memory: "32MiB"}
		}, "resources.overhead requires")
	})

	t.Run("watermark_high without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.WatermarkHigh = &WatermarkHighConfig{Memory: "256MiB"}
		}, "resources.watermark_high requires")
	})

	t.Run("startup_burst without controller", func(t *testing.T) {
		// Even with cgroup_path set, startup_burst still needs controller.
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.StartupBurst = &StartupBurstConfig{Memory: "256MiB"}
		}, "resources.startup_burst requires")
	})

	t.Run("fractional cpu without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Allocatable.CPU = 0.5 // < capacity.cpu (2)
		}, "fractional cpu requires cgroup_path")
	})

	t.Run("cgroup_path missing on disk", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = "/nonexistent/cgroup/path/abc"
		}, "does not exist")
	})

	t.Run("cgroup_path file not dir", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "notadir")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = f.Name()
		}, "is not a directory")
	})

	t.Run("watermark_high above allocatable", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			// allocatable.memory = 1GiB; set watermark_high above that
			c.Resources.WatermarkHigh = &WatermarkHighConfig{Memory: "2GiB"}
		}, "watermark_high.memory")
	})

	t.Run("startup_burst below allocatable", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			// allocatable=1GiB; startup below it
			c.Resources.StartupBurst = &StartupBurstConfig{Memory: "256MiB"}
		}, "startup_burst.memory")
	})

	t.Run("startup_burst above capacity", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			// capacity=2GiB; startup above
			c.Resources.StartupBurst = &StartupBurstConfig{Memory: "4GiB"}
		}, "startup_burst.memory")
	})

	t.Run("valid static-cgroup mode with fractional cpu", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Allocatable.CPU = 0.5
		}, "")
	})

	t.Run("valid dynamic mode with explicit startup_burst", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			c.Resources.StartupBurst = &StartupBurstConfig{Memory: "1500MiB"}
		}, "")
	})
}

func TestResourceControlDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeYAML(t, `
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 2, memory: 256MiB }
  control:
    cgroup_path: `+dir+`
    controller: /run/x.sock
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay: { diff: file:///d }
launch: { exec: /bin/true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold: %v", err)
	}

	// Overhead default = 32 MiB
	ovh, err := cfg.OverheadMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if ovh != 32<<20 {
		t.Errorf("default overhead = %d, want 32 MiB (%d)", ovh, 32<<20)
	}

	// WatermarkHigh default = allocatable * 0.875
	wm, err := cfg.WatermarkHighBytes()
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(float64(256<<20) * 0.875)
	if wm != want {
		t.Errorf("default watermark_high = %d, want %d", wm, want)
	}

	// StartupBurst default = allocatable
	sb, err := cfg.StartupBurstBytes()
	if err != nil {
		t.Fatal(err)
	}
	if sb != 256<<20 {
		t.Errorf("default startup_burst = %d, want allocatable %d", sb, 256<<20)
	}

	// DeflateOnOOM default = true
	if !cfg.DeflateOnOOM() {
		t.Errorf("default DeflateOnOOM = false, want true")
	}
}

func TestDeflateOnOOM_Override(t *testing.T) {
	cfg, err := Load(writeYAML(t, `
resources:
  capacity: { cpu: 1, memory: 1GiB }
  allocatable: { cpu: 1, memory: 256MiB, deflate_on_oom: false }
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay: { diff: file:///d }
launch: { exec: /bin/true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeflateOnOOM() {
		t.Errorf("DeflateOnOOM = true, want false (explicit override)")
	}
}

func TestCPUWeight_Mapping(t *testing.T) {
	cases := []struct {
		alloc float64
		want  uint64
	}{
		{0.001, 1},     // clamp lower
		{0.1, 10},      // 0.1 core
		{1.0, 100},     // 1 core (kernel default)
		{2.0, 200},     // 2 cores
		{50.0, 5000},   // mid-range
		{200.0, 10000}, // clamp upper
	}
	for _, tc := range cases {
		cfg := &SandboxConfig{}
		cfg.Resources.Allocatable.CPU = tc.alloc
		got := cfg.CPUWeight()
		if got != tc.want {
			t.Errorf("CPUWeight(%g) = %d, want %d", tc.alloc, got, tc.want)
		}
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
