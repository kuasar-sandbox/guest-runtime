package sandbox

import (
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandbox-runtime/pkg/config"
)

// TestBuildSnapshotCfg_SingleDisk verifies a single-disk snapshot.cfg records
// the captured diff at root.base (with the cold-start CoW lower chained into
// base_from_refs) and emits neither a base_ref nor an overlay node.
func TestBuildSnapshotCfg_SingleDisk(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb" // overlay-only; must be omitted here
	cfg.Boot.Root.DiffTemplate = "file:///root.ext4"  // Overlay nil ⇒ single-disk
	cfg.Boot.Root.Base = "manifest://coldbase"        // CoW lower → chained on cold start

	body, err := buildSnapshotCfg(cfg, "manifest://captured")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"base: manifest://captured", "base_from_refs:", "manifest://coldbase"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-disk snapshot.cfg missing %q\n%s", want, s)
		}
	}
	for _, banned := range []string{"overlay:", "base_ref:"} {
		if strings.Contains(s, banned) {
			t.Errorf("single-disk snapshot.cfg must not contain %q\n%s", banned, s)
		}
	}
}

// TestBuildSnapshotCfg_Overlay verifies overlay mode still records base_ref +
// the overlay node (the captured diff at overlay.base).
func TestBuildSnapshotCfg_Overlay(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///d.ext4"} // overlay mode

	body, err := buildSnapshotCfg(cfg, "manifest://captured")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"base_ref: file://img@sha256:bb", "overlay:", "base: manifest://captured"} {
		if !strings.Contains(s, want) {
			t.Errorf("overlay snapshot.cfg missing %q\n%s", want, s)
		}
	}
}
