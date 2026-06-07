package restore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/sandbox"
)

// SnapshotCfg mirrors the on-disk snapshot.cfg schema (docs/sandbox.md
// §3.4). Parsed from the trailing-ZIP entry "snapshot.cfg" inside a
// <sid>.snapshot bundle.
type SnapshotCfg struct {
	Resources struct {
		Capacity struct {
			CPU    int    `yaml:"cpu"`
			Memory string `yaml:"memory"`
		} `yaml:"capacity"`
	} `yaml:"resources"`
	FromRefs []string `yaml:"from_refs"` // memory chain below this bundle (§3.5)
	Boot     struct {
		RuntimeRef string `yaml:"runtime_ref"`
		Root       struct {
			BaseRef string `yaml:"base_ref"`
			Overlay struct {
				Base         string   `yaml:"base"`
				BaseFromRefs []string `yaml:"base_from_refs"` // disk chain below base (§3.5)
			} `yaml:"overlay"`
		} `yaml:"root"`
	} `yaml:"boot"`
}

// ParseSnapshotCfg parses the YAML body of a snapshot.cfg ZIP entry.
func ParseSnapshotCfg(body []byte) (*SnapshotCfg, error) {
	var cfg SnapshotCfg
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("snapshot.cfg: %w", err)
	}
	return &cfg, nil
}

// Ref is a parsed `file://<basename>@sha256:<digest>` or
// `manifest://<key>` reference.
type Ref struct {
	Scheme   string // "file" or "manifest"
	Basename string // file mode: filename only; manifest mode: empty
	Digest   string // file mode: sha256 hex (lowercase); manifest mode: empty
	Key      string // manifest mode: hex content key; file mode: empty
}

// String reconstructs the canonical form. Inverse of ParseRef.
func (r Ref) String() string {
	switch r.Scheme {
	case "file":
		return "file://" + r.Basename + "@sha256:" + r.Digest
	case "manifest":
		return "manifest://" + r.Key
	default:
		return ""
	}
}

// ParseRef parses runtime_ref / base_ref strings.
func ParseRef(s string) (Ref, error) {
	switch {
	case strings.HasPrefix(s, "manifest://"):
		key := strings.TrimPrefix(s, "manifest://")
		if key == "" {
			return Ref{}, errors.New("manifest:// ref: empty key")
		}
		return Ref{Scheme: "manifest", Key: key}, nil
	case strings.HasPrefix(s, "file://"):
		body := strings.TrimPrefix(s, "file://")
		// Expect <basename>@sha256:<hex>
		idx := strings.LastIndex(body, "@sha256:")
		if idx < 0 {
			return Ref{}, fmt.Errorf("file:// ref %q: missing @sha256:<digest>", s)
		}
		base := body[:idx]
		dig := body[idx+len("@sha256:"):]
		if base == "" || dig == "" {
			return Ref{}, fmt.Errorf("file:// ref %q: empty basename or digest", s)
		}
		return Ref{Scheme: "file", Basename: base, Digest: dig}, nil
	default:
		return Ref{}, fmt.Errorf("ref %q: missing file:// or manifest:// scheme", s)
	}
}

// ApplyRules merges host sandbox.yaml fields against the snapshot.cfg
// per docs/sandbox.md §11.0. Host-provided file:// URLs in
// boot.runtime / boot.root.base must (a) match the snapshot.cfg ref's
// scheme + basename + digest verbatim; (b) resolve to a host file with
// matching SHA256. Host-empty fields are auto-filled from snapshot.cfg
// (basename interpreted relative to the snapshot bundle's directory).
//
// Capacity must match exactly when host provides it. Network must be
// provided. boot.root.overlay.base in host yaml is silently ignored
// (always taken from snapshot.cfg). boot.kernel / boot.cmdline /
// launch.* are silently ignored.
//
// snapshotPath is the local file path of the <sid>.snapshot bundle
// (used to resolve runtime/base file basenames). Pass empty when the
// bundle came from manifest:// — host yaml must then provide all
// runtime/base ref fields explicitly.
//
// Returns the merged SandboxConfig the lifecycle should run with.
func ApplyRules(host *sandbox.SandboxConfig, snap *SnapshotCfg, snapshotPath string) (*sandbox.SandboxConfig, error) {
	if host == nil {
		return nil, errors.New("ApplyRules: host config is nil")
	}
	if snap == nil {
		return nil, errors.New("ApplyRules: snapshot.cfg is nil")
	}
	out := *host // shallow copy

	// 1. capacity: exact match if host provides; otherwise copy.
	if host.Resources.Capacity.CPU != 0 || host.Resources.Capacity.Memory != "" {
		if host.Resources.Capacity.CPU != snap.Resources.Capacity.CPU ||
			host.Resources.Capacity.Memory != snap.Resources.Capacity.Memory {
			return nil, fmt.Errorf("capacity mismatch with snapshot.cfg: host=%dvCPU/%s snap=%dvCPU/%s",
				host.Resources.Capacity.CPU, host.Resources.Capacity.Memory,
				snap.Resources.Capacity.CPU, snap.Resources.Capacity.Memory)
		}
	} else {
		out.Resources.Capacity.CPU = snap.Resources.Capacity.CPU
		out.Resources.Capacity.Memory = snap.Resources.Capacity.Memory
	}

	// 2. network: exactly one source required (same rule as cold start).
	if (host.Network.TAP == "") == (host.Network.TapFD == nil) {
		return nil, errors.New("network: exactly one of `tap` or `tapfd` is required in restore mode")
	}
	if host.Network.TapFD != nil && len(host.Network.TapFD.Exec) == 0 {
		return nil, errors.New("network.tapfd.exec is required")
	}

	// 3. boot.runtime: file:// only (cold + restore alike).
	snapRuntimeRef, err := ParseRef(snap.Boot.RuntimeRef)
	if err != nil {
		return nil, fmt.Errorf("snapshot.cfg.runtime_ref: %w", err)
	}
	if snapRuntimeRef.Scheme != "file" {
		return nil, fmt.Errorf("snapshot.cfg.runtime_ref: scheme %q unexpected (boot.runtime is file:// only)", snapRuntimeRef.Scheme)
	}
	resolvedRuntime, err := resolveBootFileRef(host.Boot.Runtime, snapRuntimeRef, snapshotPath, "boot.runtime")
	if err != nil {
		return nil, err
	}
	out.Boot.Runtime = resolvedRuntime

	// 4. boot.root.base: either file:// or manifest://.
	snapBaseRef, err := ParseRef(snap.Boot.Root.BaseRef)
	if err != nil {
		return nil, fmt.Errorf("snapshot.cfg.base_ref: %w", err)
	}
	resolvedBase, err := resolveAnyRef(host.Boot.Root.Base, snapBaseRef, snapshotPath, "boot.root.base")
	if err != nil {
		return nil, err
	}
	out.Boot.Root.Base = resolvedBase

	// 5. boot.root.overlay.base: silently ignore host yaml; always use
	//    snapshot.cfg.
	out.Boot.Root.Overlay.Base = snap.Boot.Root.Overlay.Base

	// 6. boot.root.overlay.diff: optional (host-localized). Empty → restore
	// auto-defaults a fresh diff under the on-disk base dir (restore.go),
	// sized to the snapshot's overlay base.

	// boot.kernel / boot.cmdline / launch.* silently ignored — fields
	// stay as host yaml provided, but lifecycle.go won't use them on
	// the restore path.

	return &out, nil
}

// resolveBootFileRef enforces the file:// rules for boot.runtime /
// boot.root.base when the snapshot.cfg ref is file:// type.
//
//   - host empty: build absolute path = filepath.Join(<bundle dir>, basename),
//     verify file SHA256 matches digest, return file:// + abs path.
//   - host non-empty: parse host URL → file path, verify
//     basename(path) == ref.Basename, sha256(file) == ref.Digest,
//     return host URL as-is.
func resolveBootFileRef(hostURL string, snapRef Ref, snapshotPath, fieldName string) (string, error) {
	if hostURL == "" {
		if snapshotPath == "" {
			return "", fmt.Errorf("%s: snapshot.cfg ref is file:// but bundle is manifest-loaded; provide %s explicitly", fieldName, fieldName)
		}
		bundleDir := filepath.Dir(snapshotPath)
		abs := filepath.Join(bundleDir, snapRef.Basename)
		if err := verifyFileDigest(abs, snapRef.Digest); err != nil {
			return "", fmt.Errorf("%s: auto-resolved %s: %w", fieldName, abs, err)
		}
		return "file://" + abs, nil
	}

	if !strings.HasPrefix(hostURL, "file://") {
		return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=file://)", fieldName, hostURL)
	}
	hostPath := strings.TrimPrefix(hostURL, "file://")
	if !filepath.IsAbs(hostPath) {
		return "", fmt.Errorf("%s: file:// must be absolute (got %q)", fieldName, hostURL)
	}
	if filepath.Base(hostPath) != snapRef.Basename {
		return "", fmt.Errorf("%s: basename mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, filepath.Base(hostPath), snapRef.Basename)
	}
	if err := verifyFileDigest(hostPath, snapRef.Digest); err != nil {
		return "", fmt.Errorf("%s: digest mismatch: %w", fieldName, err)
	}
	return hostURL, nil
}

// resolveAnyRef handles either file:// or manifest:// refs (used by
// boot.root.base which accepts both).
func resolveAnyRef(hostURL string, snapRef Ref, snapshotPath, fieldName string) (string, error) {
	switch snapRef.Scheme {
	case "file":
		return resolveBootFileRef(hostURL, snapRef, snapshotPath, fieldName)
	case "manifest":
		if hostURL == "" {
			return snapRef.String(), nil
		}
		if !strings.HasPrefix(hostURL, "manifest://") {
			return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=manifest://)", fieldName, hostURL)
		}
		hostKey := strings.TrimPrefix(hostURL, "manifest://")
		if hostKey != snapRef.Key {
			return "", fmt.Errorf("%s: manifest key mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, hostKey, snapRef.Key)
		}
		return hostURL, nil
	default:
		return "", fmt.Errorf("%s: unsupported scheme %q in snapshot.cfg", fieldName, snapRef.Scheme)
	}
}

// verifyFileDigest streams the file at path, computes SHA256 (sparse
// holes read as 0), and returns nil iff hex(hash) == want.
func verifyFileDigest(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch (got %s, want %s)", got, want)
	}
	return nil
}
