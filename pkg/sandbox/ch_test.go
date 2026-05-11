package sandbox

import (
	"strings"
	"testing"
)

func makeMinimalCfg() *SandboxConfig {
	cfg := &SandboxConfig{
		Resources: ResourcesConfig{
			Capacity:    CapacityConfig{CPU: 2, Memory: "4GiB"},
			Allocatable: AllocatableConfig{CPU: 1.5, Memory: "2GiB"},
		},
		Network: NetworkConfig{TAP: "tap0"},
		Boot: BootConfig{
			Kernel:  "file:///vmlinux",
			Runtime: "file:///sandbox-runtime.erofs",
			Cmdline: "console=hvc0",
			Root: RootConfig{
				Base: "file:///c.erofs",
				Overlay: OverlayConfig{
					Diff: "file:///d.ext4",
					Size: "1GiB",
				},
			},
		},
		Launch: LaunchConfig{
			Exec:    "/usr/bin/echo",
			Args:    []string{"hello", "world"},
			Workdir: "/",
			Restart: "never",
		},
	}
	cfg.applyDefaults()
	return cfg
}

func TestCHCommand_HasExpectedFlags(t *testing.T) {
	cfg := makeMinimalCfg()
	args, err := CHCommand(cfg,
		"/run/sb/blk0.sock", "/run/sb/blk1.sock", "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock")
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--api-socket /run/sb/ch.sock",
		"--kernel /vmlinux",
		"file=/sandbox-runtime.erofs,discard_writes=on",
		"size=4096M,shared=on,fd=3,uffd_socket=/run/sb/uffd.sock",
		"--balloon size=0",
		"boot=2",
		"--disk vhost_user=on,socket=/run/sb/blk0.sock,readonly=on vhost_user=on,socket=/run/sb/blk1.sock",
		"tap=tap0",
		"cid=3,socket=/run/sb/vsock.sock", // vsock device
		"console=hvc0",
		"init=/sbin/init",
		"root=/dev/pmem0",
		"rootfstype=erofs",
		"dax=always",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("CH cmdline missing %q\n  got: %s", want, joined)
		}
	}
	// Ensure launch params are NOT in the cmdline (they go via vsock).
	for _, banned := range []string{"sandbox.app", "sandbox.args", "sandbox.env", "sandbox.workdir"} {
		if strings.Contains(joined, banned) {
			t.Errorf("CH cmdline must not contain %q (launch goes via vsock now)", banned)
		}
	}
}

func TestCHCommand_BalloonDeflateOnOOMDefault(t *testing.T) {
	cfg := makeMinimalCfg()
	args, err := CHCommand(cfg, "/0", "/1", "/c", "/v", "/k", "/r", "/u")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "deflate_on_oom=on") {
		t.Errorf("balloon should contain deflate_on_oom=on by default, got: %s", joined)
	}
}

func TestCHCommand_BalloonDeflateOnOOMDisabled(t *testing.T) {
	cfg := makeMinimalCfg()
	off := false
	cfg.Resources.Allocatable.DeflateOnOOM = &off
	args, err := CHCommand(cfg, "/0", "/1", "/c", "/v", "/k", "/r", "/u")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "deflate_on_oom=on") {
		t.Errorf("balloon should NOT contain deflate_on_oom=on when explicitly disabled, got: %s", joined)
	}
	// free_page_reporting is now intentionally OFF (its mmu_notifier
	// traffic deadlocks the guest vsock kthread; replaced by the
	// host-side BalloonController + sandbox-init mem_report).
	if strings.Contains(joined, "free_page_reporting") {
		t.Errorf("balloon must not advertise free_page_reporting (replaced by mem_report-driven vm.resize), got: %s", joined)
	}
}

func TestCHCommand_NoBalloonWhenAllocEqualsCapacity(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Resources.Allocatable.Memory = cfg.Resources.Capacity.Memory
	args, err := CHCommand(cfg,
		"/run/sb/blk0.sock", "/run/sb/blk1.sock", "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if strings.Contains(a, "balloon") {
			t.Errorf("balloon should not appear when allocatable == capacity, got %q", a)
		}
	}
}

func TestBuildCmdline_ContainsAutoInjectedAndUserExtras(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Boot.Cmdline = "console=hvc0 ip=169.254.1.1::169.254.1.0:255.255.255.254:sb1:eth0:off"

	cl := buildCmdline(cfg)
	for _, want := range []string{
		"init=/sbin/init",
		"root=/dev/pmem0",
		"rootfstype=erofs",
		"dax=always",
		"console=hvc0",
		"ip=169.254.1.1",
	} {
		if !strings.Contains(cl, want) {
			t.Errorf("cmdline missing %q\n  got: %s", want, cl)
		}
	}
	for _, banned := range []string{"sandbox.app", "sandbox.args"} {
		if strings.Contains(cl, banned) {
			t.Errorf("cmdline must not contain %q (vsock-only now)", banned)
		}
	}
}

func TestCHCommand_MemoryStringsHonored(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Resources.Capacity.Memory = "512MiB"
	cfg.Resources.Allocatable.Memory = "256MiB"
	args, err := CHCommand(cfg, "/0", "/1", "/c", "/v", "/k", "/r", "/u")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "size=512M,shared=on") {
		t.Errorf("expected 512M memory, got: %s", joined)
	}
	// Balloon starts at size=0 and is driven up to (cap-alloc) post-Settled
	// by the host BalloonController. The cmdline carries size=0 always.
	if !strings.Contains(joined, "--balloon size=0") {
		t.Errorf("expected --balloon size=0 (boot value; controller drives to target), got: %s", joined)
	}
}
