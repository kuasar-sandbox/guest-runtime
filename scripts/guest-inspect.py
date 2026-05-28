#!/usr/bin/env python3
"""
guest-inspect.py — read guest kernel memory counters from host.

Reads `/proc/<CH-PID>/mem` and uses vmlinux symbols + region table from
the sandbox-ctl boot log to decode vm_zone_stat, vm_node_stat, and
per-cpu vm_event_states.

Run as root (needs CAP_SYS_PTRACE for /proc/<pid>/mem):

    sudo python3 scripts/guest-inspect.py \
        --log build/test.log \
        --vmlinux bin/vmlinux

The kernel is built with KASLR off so all symbols are at fixed virtual
addresses (deps/vmlinux/sandbox-common.config: CONFIG_RANDOMIZE_BASE
not set, CONFIG_RANDOMIZE_MEMORY not set).
"""

import os
import re
import struct
import subprocess
import sys
import argparse
import json
import time

# x86_64 kernel virt layout (no KASLR, 4-level paging).
KERNEL_IMAGE_BASE = 0xffffffff80000000  # __START_KERNEL_map
PAGE_OFFSET        = 0xffff888000000000  # __PAGE_OFFSET / direct map base
DIRECTMAP_END      = 0xffffc88000000000  # __PAGE_OFFSET + 64 TiB

# Guest physical memory layout under cloud-hypervisor with 8 GiB:
#   phys [0, 3 GiB)         → low RAM   (memfd_off 0)
#   phys [3 GiB, 4 GiB)     → PCI hole  (not backed)
#   phys [4 GiB, 9 GiB)     → high RAM  (memfd_off 3 GiB)
LOW_RAM_END  = 0xc0000000   # 3 GiB
HIGH_RAM_BASE = 0x100000000 # 4 GiB


def parse_log(path):
    """Pull CH pid + memory regions out of the latest CH boot in the log."""
    ch_pid = None
    regions = []
    re_ch = re.compile(r"CH started pid=(\d+)")
    re_va = re.compile(
        r"vareport: accepted region #(\d+) zone=\S+ va=0x([0-9a-f]+) size=(\d+) memfd_off=0x([0-9a-f]+)"
    )
    with open(path) as f:
        for line in f:
            m = re_ch.search(line)
            if m:
                ch_pid = int(m.group(1))
                regions = []
                continue
            m = re_va.search(line)
            if m:
                regions.append({
                    "idx": int(m.group(1)),
                    "va": int(m.group(2), 16),
                    "size": int(m.group(3)),
                    "memfd_off": int(m.group(4), 16),
                })
    return ch_pid, regions


def load_syms(vmlinux):
    """Read needed symbol addresses from vmlinux via `nm`."""
    want = {
        "vmstat_text", "vm_zone_stat", "vm_node_stat",
        "vm_event_states", "__per_cpu_offset", "nr_cpu_ids",
        "_text", "_etext", "__start_rodata", "_edata",
    }
    syms = {}
    out = subprocess.check_output(["nm", vmlinux]).decode().splitlines()
    for line in out:
        parts = line.split()
        if len(parts) < 3:
            continue
        addr_s, _, name = parts[0], parts[1], parts[2]
        if name in want:
            syms[name] = int(addr_s, 16)
    missing = want - syms.keys()
    if missing:
        raise SystemExit(f"missing symbols: {missing}")
    return syms


class Memory:
    """virt → guest phys → memfd offset → CH process VA → pread"""

    def __init__(self, ch_pid, regions):
        self.fd = os.open(f"/proc/{ch_pid}/mem", os.O_RDONLY)
        self.regions = sorted(regions, key=lambda r: r["memfd_off"])

    def close(self):
        os.close(self.fd)

    def _phys_to_memfd_off(self, phys):
        if phys < LOW_RAM_END:
            return phys
        if phys >= HIGH_RAM_BASE:
            return phys - HIGH_RAM_BASE + LOW_RAM_END
        raise ValueError(f"phys 0x{phys:x} is in PCI hole [{hex(LOW_RAM_END)}, {hex(HIGH_RAM_BASE)})")

    def _memfd_off_to_ch_va(self, off):
        for r in self.regions:
            if r["memfd_off"] <= off < r["memfd_off"] + r["size"]:
                return r["va"] + (off - r["memfd_off"])
        raise ValueError(f"memfd_off 0x{off:x} not in any region")

    def virt_to_ch_va(self, virt):
        if virt >= KERNEL_IMAGE_BASE:
            phys = virt - KERNEL_IMAGE_BASE
        elif PAGE_OFFSET <= virt < DIRECTMAP_END:
            phys = virt - PAGE_OFFSET
        else:
            raise ValueError(f"virt 0x{virt:016x} outside known kernel ranges")
        memfd_off = self._phys_to_memfd_off(phys)
        return self._memfd_off_to_ch_va(memfd_off)

    def pread_virt(self, virt, size):
        return os.pread(self.fd, size, self.virt_to_ch_va(virt))

    def u64(self, virt): return struct.unpack("<Q", self.pread_virt(virt, 8))[0]
    def i64(self, virt): return struct.unpack("<q", self.pread_virt(virt, 8))[0]
    def u32(self, virt): return struct.unpack("<I", self.pread_virt(virt, 4))[0]

    def cstring(self, virt, cap=128):
        data = self.pread_virt(virt, cap)
        nul = data.find(b"\0")
        return data[:nul if nul >= 0 else cap].decode("latin1", errors="replace")


def label_layout(names):
    """
    Walk vmstat_text strings to find the boundaries between the four
    blocks. Tested against Linux 6.1.169 sandbox kernel (no NUMA).
    Boundaries by name marker:
      - zone block: names[0..first node start - 1]
      - numa events (CONFIG_NUMA only): names with "numa_" prefix
      - node block: ...
      - writeback (2 entries): "nr_dirty_threshold", "nr_dirty_background_threshold"
      - vm events: rest
    """
    # Find writeback boundary.
    wb_start = None
    for i, n in enumerate(names):
        if n == "nr_dirty_threshold":
            wb_start = i
            break
    if wb_start is None:
        raise SystemExit("vmstat_text: cannot find 'nr_dirty_threshold' marker")
    # NUMA detection: any name starting with "numa_" before wb_start.
    numa_start = None
    numa_end = None
    for i in range(wb_start):
        if names[i].startswith("numa_"):
            if numa_start is None:
                numa_start = i
            numa_end = i + 1
    if numa_start is None:
        n_zone = None
        # Find boundary between zone stats and node stats. Zone names are
        # nr_free_pages, nr_zone_*, nr_mlock, nr_bounce, nr_zspages,
        # nr_free_cma. Node names start with nr_inactive_anon. Easiest:
        # find first index where name matches a known node-start marker.
        for i, n in enumerate(names[:wb_start]):
            if n == "nr_inactive_anon":
                n_zone = i
                break
        if n_zone is None:
            raise SystemExit("vmstat_text: cannot find zone/node boundary "
                             "('nr_inactive_anon' not in first block)")
        return {
            "zone_range": (0, n_zone),
            "numa_range": (n_zone, n_zone),
            "node_range": (n_zone, wb_start),
            "writeback_range": (wb_start, wb_start + 2),
            "event_range": (wb_start + 2, len(names)),
        }
    return {
        "zone_range": (0, numa_start),
        "numa_range": (numa_start, numa_end),
        "node_range": (numa_end, wb_start),
        "writeback_range": (wb_start, wb_start + 2),
        "event_range": (wb_start + 2, len(names)),
    }


def discover_nr_cpus(mem, syms, cap=512):
    """Count CPUs whose per-cpu area is set up. __per_cpu_offset[cpu]
    is a valid pointer for online CPUs and 0 (or garbage outside the
    directmap) otherwise. Cap by nr_cpu_ids, and only count entries
    whose value lands in the kernel directmap or image — anything
    else is uninitialised slot garbage (the earlier bug counted those)."""
    nr_cpu_ids = mem.u32(syms["nr_cpu_ids"])
    limit = min(cap, max(nr_cpu_ids, 1))
    n = 0
    for i in range(limit):
        v = mem.u64(syms["__per_cpu_offset"] + i * 8)
        in_directmap = PAGE_OFFSET <= v < DIRECTMAP_END
        in_image = v >= KERNEL_IMAGE_BASE
        if v == 0 or not (in_directmap or in_image):
            break
        n += 1
    return n


def dump(mem, syms, n_vmstat):
    # 1) Names from vmstat_text. Read pointers, follow each to a C string.
    names = []
    blob = mem.pread_virt(syms["vmstat_text"], n_vmstat * 8)
    for i in range(n_vmstat):
        p = struct.unpack_from("<Q", blob, i * 8)[0]
        if p == 0:
            names.append(None)
            continue
        try:
            names.append(mem.cstring(p))
        except Exception as e:
            names.append(f"<err:{i}:{e}>")
    # Trim trailing NULLs to find actual array length.
    while names and names[-1] is None:
        names.pop()

    layout = label_layout(names)
    zr, nr, ndr, wr, er = (layout["zone_range"], layout["numa_range"],
                           layout["node_range"], layout["writeback_range"],
                           layout["event_range"])
    n_zone = zr[1] - zr[0]
    n_node = ndr[1] - ndr[0]
    n_event = er[1] - er[0]

    # 2) Read zone + node arrays. Memory layout may have cacheline padding
    # past the named count; we only read named entries.
    zone_vals = [mem.i64(syms["vm_zone_stat"] + i * 8) for i in range(n_zone)]
    node_vals = [mem.i64(syms["vm_node_stat"] + i * 8) for i in range(n_node)]

    # 3) Per-CPU vm_event_states — sum across CPUs.
    n_cpus_probed = discover_nr_cpus(mem, syms)
    nr_cpu_ids = mem.u32(syms["nr_cpu_ids"])
    event_vals = [0] * n_event
    per_cpu_status = []
    for cpu in range(n_cpus_probed):
        offset = mem.u64(syms["__per_cpu_offset"] + cpu * 8)
        cpu_var_virt = syms["vm_event_states"] + offset
        try:
            data = mem.pread_virt(cpu_var_virt, n_event * 8)
            for i in range(n_event):
                event_vals[i] += struct.unpack_from("<Q", data, i * 8)[0]
            per_cpu_status.append(
                f"cpu{cpu} ok: per_cpu_offset=0x{offset:016x} "
                f"virt=0x{cpu_var_virt:016x}")
        except Exception as e:
            per_cpu_status.append(
                f"cpu{cpu} skip: per_cpu_offset=0x{offset:016x} "
                f"virt=0x{cpu_var_virt:016x} ({e})")

    return {
        "meta": {
            "nr_cpu_ids_field": nr_cpu_ids,
            "nr_cpus_probed": n_cpus_probed,
            "vmstat_text_entries": n_vmstat,
            "n_zone": n_zone,
            "n_node": n_node,
            "n_event": n_event,
            "per_cpu_status": per_cpu_status,
            "regions": mem.regions,
        },
        "zone_stat": dict(zip(names[zr[0]:zr[1]], zone_vals)),
        "node_stat": dict(zip(names[ndr[0]:ndr[1]], node_vals)),
        "vm_events": dict(zip(names[er[0]:er[1]], event_vals)),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--log", required=True)
    ap.add_argument("--vmlinux", required=True)
    ap.add_argument("--vmstat-entries", type=int, default=128)
    ap.add_argument("--watch", type=float, default=0)
    ap.add_argument("--out", default=None)
    args = ap.parse_args()

    ch_pid, regions = parse_log(args.log)
    if not ch_pid:
        sys.exit("no `CH started pid=` line in log")
    if len(regions) < 2:
        sys.exit(f"expected ≥2 memory regions, found {len(regions)}")
    syms = load_syms(args.vmlinux)

    print(f"ch_pid={ch_pid}", file=sys.stderr)
    for r in regions:
        print(f"  region {r['idx']}: va=0x{r['va']:x} size={r['size']} "
              f"memfd_off=0x{r['memfd_off']:x}", file=sys.stderr)

    mem = Memory(ch_pid, regions)

    def sample():
        s = dump(mem, syms, args.vmstat_entries)
        s["sampled_at"] = time.time()
        return s

    def flush(snaps):
        if not args.out:
            return
        # Atomic-ish: write to tmp then rename, so a reader never sees a
        # half-written file and a crash mid-write can't truncate prior data.
        tmp = args.out + ".tmp"
        with open(tmp, "w") as f:
            json.dump(snaps if args.watch > 0 else snaps[0], f, indent=2)
        os.replace(tmp, args.out)

    def heartbeat(i, s):
        z = s["zone_stat"]
        e = s["vm_events"]
        # One-line liveness so the user sees progress and the key signals
        # without waiting for Ctrl-C.
        print(
            f"[{i:4d}] free={z.get('nr_free_pages',0)}p "
            f"inflate={e.get('balloon_inflate',0)} deflate={e.get('balloon_deflate',0)} "
            f"oom={e.get('oom_kill',0)} "
            f"pgsteal_d={e.get('pgsteal_direct',0)} pgscan_d={e.get('pgscan_direct',0)} "
            f"allocstall_n={e.get('allocstall_normal',0)}",
            file=sys.stderr, flush=True)

    snaps = []
    try:
        if args.watch <= 0:
            s = sample()
            snaps.append(s)
            flush(snaps)
        else:
            i = 0
            while True:
                s = sample()
                snaps.append(s)
                flush(snaps)          # incremental: survives crash / no Ctrl-C
                heartbeat(i, s)
                i += 1
                time.sleep(args.watch)
    except KeyboardInterrupt:
        pass
    finally:
        mem.close()
        flush(snaps)

    if args.out:
        print(f"wrote {args.out} ({len(snaps)} sample(s))", file=sys.stderr)
    elif snaps:
        print(json.dumps(snaps if args.watch > 0 else snaps[0], indent=2))


if __name__ == "__main__":
    main()
