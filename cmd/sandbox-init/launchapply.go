// Launch-spec application: declarative mounts, file injection, one-shot
// init commands, and run-as-user resolution. All via raw syscalls — no
// external tools (docs/sandbox-runtime.md §3.1-§3.2).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox/proto"
	"github.com/moby/sys/user"
	"golang.org/x/sys/unix"
)

const fileInjectStage = "/run/.inject"

// applyVolumeMounts sets up `empty` (volume) mounts BEFORE switch-root: the
// source is a fresh dir on the raw ext4 (/mnt/upper/volumes/<i>, sibling to
// the overlay upperdir), bound onto the target inside /mnt/newroot. The
// switch-root MS_MOVE then carries the bind into the new /. The bind keeps
// the ext4 source alive after /mnt/upper becomes unreachable, and masks any
// image content at the target. Disk-backed (vdb), so it does not consume RAM.
func applyVolumeMounts(mounts []proto.MountSpec) error {
	vi := 0
	for _, m := range mounts {
		if m.Type != "empty" {
			continue
		}
		src := fmt.Sprintf("/mnt/upper/volumes/%d", vi)
		vi++
		if err := os.MkdirAll(src, 0o755); err != nil {
			return fmt.Errorf("mkdir volume source %s: %w", src, err)
		}
		tgt := "/mnt/newroot" + m.Target
		if err := os.MkdirAll(tgt, 0o755); err != nil {
			return fmt.Errorf("mkdir volume target %s: %w", tgt, err)
		}
		if err := unix.Mount(src, tgt, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind volume -> %s: %w", m.Target, err)
		}
	}
	return nil
}

// applyFsMounts mounts the `tmpfs` entries AFTER switch-root (independent
// filesystems, no raw-ext4 source needed). `empty` entries are skipped here
// (handled pre-switch by applyVolumeMounts).
func applyFsMounts(mounts []proto.MountSpec) error {
	for _, m := range mounts {
		if m.Type != "tmpfs" {
			continue
		}
		flags, data := parseMountOptions(m.Options)
		if err := os.MkdirAll(m.Target, 0o755); err != nil {
			return fmt.Errorf("mkdir mount target %s: %w", m.Target, err)
		}
		if err := unix.Mount("tmpfs", m.Target, "tmpfs", flags, data); err != nil {
			return fmt.Errorf("mount tmpfs %s: %w", m.Target, err)
		}
	}
	return nil
}

// parseMountOptions splits a mount option string into MS_* flags and the
// residual data string (mode=, size=, ...). Unknown tokens go to data.
func parseMountOptions(opts string) (uintptr, string) {
	if opts == "" {
		return 0, ""
	}
	var flags uintptr
	var data []string
	for _, o := range strings.Split(opts, ",") {
		switch strings.TrimSpace(o) {
		case "":
		case "rw":
		case "ro":
			flags |= unix.MS_RDONLY
		case "nosuid":
			flags |= unix.MS_NOSUID
		case "nodev":
			flags |= unix.MS_NODEV
		case "noexec":
			flags |= unix.MS_NOEXEC
		case "noatime":
			flags |= unix.MS_NOATIME
		case "nodiratime":
			flags |= unix.MS_NODIRATIME
		case "relatime":
			flags |= unix.MS_RELATIME
		case "sync":
			flags |= unix.MS_SYNCHRONOUS
		default:
			data = append(data, strings.TrimSpace(o))
		}
	}
	return flags, strings.Join(data, ",")
}

// applyFiles injects files via a private staging tmpfs + per-file bind:
// content is written to /run/.inject/<i> (memory only), then bind-mounted
// onto the target path, then the staging mount is lazily detached (the
// per-file binds keep the tmpfs inodes alive). Content never lands on the
// writable disk, so this is safe for secrets. Used both at cold start and
// at restore (per-instance injection). ReadOnly → bind then remount RO.
func applyFiles(files []proto.FileSpec) error {
	if len(files) == 0 {
		return nil
	}
	if err := os.MkdirAll(fileInjectStage, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", fileInjectStage, err)
	}
	if err := unix.Mount("tmpfs", fileInjectStage, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700"); err != nil {
		return fmt.Errorf("mount inject tmpfs: %w", err)
	}
	defer func() { _ = unix.Unmount(fileInjectStage, unix.MNT_DETACH) }()

	for i, f := range files {
		src := fmt.Sprintf("%s/%d", fileInjectStage, i)
		mode := parseFileMode(f.Mode, 0o644)
		if err := os.WriteFile(src, []byte(f.Content), mode); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		// WriteFile applies umask; force the requested mode (e.g. 0400 secrets).
		if err := os.Chmod(src, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", f.Path, err)
		}
		uid, gid, err := resolveOwner(f.Owner)
		if err != nil {
			return fmt.Errorf("file %s owner: %w", f.Path, err)
		}
		if err := os.Chown(src, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", f.Path, err)
		}
		if err := ensureTargetFile(f.Path); err != nil {
			return fmt.Errorf("prepare target %s: %w", f.Path, err)
		}
		if err := unix.Mount(src, f.Path, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind file %s: %w", f.Path, err)
		}
		if f.ReadOnly {
			if err := unix.Mount("", f.Path, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("remount ro %s: %w", f.Path, err)
			}
		}
	}
	return nil
}

// ensureTargetFile makes sure path's parent exists and path exists as a file
// to bind over (a bind mount needs an existing target). If path already
// exists (file or symlink), we bind over it as-is (masking image content).
func ensureTargetFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func parseFileMode(s string, def os.FileMode) os.FileMode {
	if s == "" {
		return def
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return def
	}
	return os.FileMode(n)
}

// runInit runs each one-shot init command in order, to completion. They run
// in PID 1's existing namespaces (post switch-root) with output to the guest
// console. A per-command run-as user (init[].user) is applied via the child
// credential (safe here — no /proc remount is involved, unlike the app fork).
// A non-zero exit aborts startup (initContainers semantics).
func runInit(specs []proto.InitSpec) error {
	for i, s := range specs {
		cmd := exec.Command(s.Exec, s.Args...)
		cmd.Stdin = nil
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Env = envSliceFromMap(nil)
		if s.User != "" {
			c, err := resolveCred(s.User)
			if err != nil {
				return fmt.Errorf("init[%d] user %q: %w", i, s.User, err)
			}
			if c != nil {
				cmd.SysProcAttr = &syscall.SysProcAttr{
					Credential: &syscall.Credential{Uid: c.uid, Gid: c.gid, Groups: c.sgids},
				}
			}
		}
		logf("init[%d]: %s %v", i, s.Exec, s.Args)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("init[%d] %q: %w", i, s.Exec, err)
		}
	}
	return nil
}

// --- run-as-user resolution (guest-side /etc/passwd + /etc/group) ---------

// ugCred is a resolved run-as identity.
type ugCred struct {
	uid, gid uint32
	sgids    []uint32
}

// encode serializes to the "uid:gid:sg1,sg2" form passed to the exec-child
// re-exec (which drops privileges just before execve). "-" means no drop.
func (c *ugCred) encode() string {
	sg := make([]string, len(c.sgids))
	for i, g := range c.sgids {
		sg[i] = strconv.FormatUint(uint64(g), 10)
	}
	return fmt.Sprintf("%d:%d:%s", c.uid, c.gid, strings.Join(sg, ","))
}

// resolveCred resolves a "uid:gid" / "name:group" / "name" / "uid" spec
// against the rootfs /etc/passwd and /etc/group via moby/sys/user (the
// runc/containerd resolver). Numeric forms work without a passwd entry;
// supplementary groups come from group membership; missing files are
// tolerated. Empty spec → nil (no drop).
func resolveCred(spec string) (*ugCred, error) {
	if spec == "" {
		return nil, nil
	}
	eu, err := user.GetExecUserPath(spec, nil, "/etc/passwd", "/etc/group")
	if err != nil {
		return nil, err
	}
	sgids := make([]uint32, len(eu.Sgids))
	for i, g := range eu.Sgids {
		sgids[i] = uint32(g)
	}
	return &ugCred{uid: uint32(eu.Uid), gid: uint32(eu.Gid), sgids: sgids}, nil
}

// resolveOwner resolves a file owner spec to numeric uid/gid (empty → 0:0).
func resolveOwner(spec string) (int, int, error) {
	if spec == "" {
		return 0, 0, nil
	}
	c, err := resolveCred(spec)
	if err != nil {
		return 0, 0, err
	}
	return int(c.uid), int(c.gid), nil
}

// applyCred drops privileges in the exec-child just before execve, given the
// "uid:gid:sg1,sg2" string. "-"/"" → no-op. Uses the stdlib all-threads
// Set{groups,gid,uid} so the drop is process-wide. Order: groups, gid, uid.
func applyCred(cred string) error {
	if cred == "" || cred == "-" {
		return nil
	}
	parts := strings.SplitN(cred, ":", 3)
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("bad cred uid in %q", cred)
	}
	gid := uid
	if len(parts) >= 2 && parts[1] != "" {
		if g, err := strconv.Atoi(parts[1]); err == nil {
			gid = g
		}
	}
	var groups []int
	if len(parts) >= 3 && parts[2] != "" {
		for _, g := range strings.Split(parts[2], ",") {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, n)
			}
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	return nil
}
