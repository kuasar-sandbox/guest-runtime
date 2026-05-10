package sandbox

import (
	"fmt"
	"strings"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// CHCommand assembles the cloud-hypervisor argv for a cold-start sandbox.
//
// vsockSock is the host-side base UDS path; CH proxies guest CID 2 vsock
// traffic to "<vsockSock>_<port>" entries. sandbox-ctl listens on the
// per-port suffix (proto.LaunchPort) for the launch handshake.
//
// Memory sizing: --memory size= is the capacity (what guest sees), with
// shared=on so vhost-user backends in the same process can mmap the
// memfd CH creates. Balloon size = capacity - allocatable, releasing the
// difference back to host at boot; free_page_reporting=on lets the guest
// continuously report unused pages (drives EVENT_REMOVE on the uffd).
//
// uffdSock is the path of the va_report UDS server (cloud-hypervisor.md
// §3.3). Patched CH connects to it during create_ram_region.
//
// The memfd fd and uffd fd are inherited via cmd.ExtraFiles; CH sees
// them at fd=3 and fd=4 respectively, referenced in --memory-zone.
func CHCommand(cfg *SandboxConfig, blk0Sock, blk1Sock, chSock, vsockSock, kernelPath, runtimePath, uffdSock string) ([]string, error) {
	capBytes, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return nil, err
	}
	allocBytes, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		return nil, err
	}

	// --memory-zone replaces --memory under the unified-memfd model:
	// fd=3 is the memfd inherited via cmd.ExtraFiles[0]. uffd_socket
	// is the path of the va_report UDS server in sandbox-ctl; CH
	// creates its own uffd in create_ram_region (mm-bound to CH so
	// faults route correctly) and hands the fd back via SCM_RIGHTS.
	memZone := fmt.Sprintf(
		"id=ram0,size=%dM,shared=on,fd=3,uffd_socket=%s",
		capBytes>>20, uffdSock)

	args := []string{
		"--api-socket", chSock,
		"--kernel", kernelPath,
		"--pmem", fmt.Sprintf("file=%s,discard_writes=on,iommu=off", runtimePath),
		// CH 51 requires --memory size=0 when zones are used; the size
		// is taken from the zone config.
		"--memory", "size=0,shared=on",
		"--memory-zone", memZone,
		"--cpus", fmt.Sprintf("boot=%d", cfg.Resources.Capacity.CPU),
		"--disk",
		fmt.Sprintf("vhost_user=on,socket=%s,readonly=on", blk0Sock),
		fmt.Sprintf("vhost_user=on,socket=%s", blk1Sock),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", proto.VsockGuestCID, vsockSock),
		"--console", "tty",
		"--serial", "null",
	}

	if allocBytes < capBytes {
		balloonBytes := capBytes - allocBytes
		balloonOpts := fmt.Sprintf("size=%dM,free_page_reporting=on", balloonBytes>>20)
		if cfg.DeflateOnOOM() {
			balloonOpts += ",deflate_on_oom=on"
		}
		args = append(args, "--balloon", balloonOpts)
	}

	if cfg.Network.TAP != "" {
		args = append(args, "--net", fmt.Sprintf("tap=%s,iommu=off", cfg.Network.TAP))
	}

	args = append(args, "--cmdline", buildCmdline(cfg))
	return args, nil
}

// buildCmdline returns the kernel cmdline for guest boot.
//
// Layout:
//   1. Auto-injected fixed boot params (init, root, rootfstype, etc.) —
//      lock down how sandbox-runtime.erofs is mounted as / via virtio-pmem.
//   2. User-supplied boot.cmdline extras (console=, ip=, …).
//
// The launch spec (exec/args/env/workdir/restart) is **not** in the
// cmdline anymore — it travels over vsock at runtime via the launch
// protocol (pkg/sandbox/proto). This avoids cmdline length limits and
// shell-quoting issues for multi-arg / multi-env launches.
func buildCmdline(cfg *SandboxConfig) string {
	parts := []string{
		"init=/sbin/init",
		"root=/dev/pmem0",
		"ro",
		"rootfstype=erofs",
		"dax=always",
	}
	if cfg.Boot.Cmdline != "" {
		parts = append(parts, cfg.Boot.Cmdline)
	}
	return strings.Join(parts, " ")
}
