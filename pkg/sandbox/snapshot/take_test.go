package snapshot

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// patchSnapshotCfgYAML must replace overlay.base while leaving every
// other field intact. Verifies the rendezvous between Take() (which
// emits the initial doc) and Upload() (which patches it post-ingest).
func TestPatchSnapshotCfgYAML_OverlayOnly(t *testing.T) {
	src := []byte(`resources:
  capacity:
    cpu: 2
    memory: 4GiB
boot:
  runtime_ref: file://runtime.erofs@sha256:abc
  root:
    base_ref: manifest://deadbeef
    overlay:
      base: file://placeholder.overlay
`)
	patched, err := patchSnapshotCfgYAML(src, "manifest://0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(patched, &doc); err != nil {
		t.Fatal(err)
	}
	res, _ := doc["resources"].(map[string]any)
	cap, _ := res["capacity"].(map[string]any)
	if cap["cpu"] != 2 || cap["memory"] != "4GiB" {
		t.Errorf("capacity altered: %+v", cap)
	}
	boot, _ := doc["boot"].(map[string]any)
	if boot["runtime_ref"] != "file://runtime.erofs@sha256:abc" {
		t.Errorf("runtime_ref altered: %v", boot["runtime_ref"])
	}
	root, _ := boot["root"].(map[string]any)
	if root["base_ref"] != "manifest://deadbeef" {
		t.Errorf("base_ref altered: %v", root["base_ref"])
	}
	overlay, _ := root["overlay"].(map[string]any)
	if overlay["base"] != "manifest://0123456789abcdef" {
		t.Errorf("overlay.base = %v, want manifest://0123456789abcdef", overlay["base"])
	}
}

func TestPatchSnapshotCfgYAML_FillsMissingTree(t *testing.T) {
	// Edge case: input doc has only resources, no boot block.
	// Patch should still inject boot.root.overlay.base.
	src := []byte("resources:\n  capacity:\n    cpu: 1\n    memory: 1GiB\n")
	patched, err := patchSnapshotCfgYAML(src, "file://abc.overlay")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "base: file://abc.overlay") {
		t.Errorf("missing overlay.base in patched: %s", patched)
	}
}
