package restore

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/config"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/sandbox-accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandbox-accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandbox-runtime/internal/util"
	"github.com/kuasar-sandbox/sandbox-runtime/pkg/snapshot"
	"gopkg.in/yaml.v3"
)

// UploadLocal promotes a LOCAL file snapshot (the sparse <sid>.snapshot bundle +
// its <sha>.overlay, carrying a snapshot.cfg layer chain) to a REMOTE manifest://
// snapshot WITHOUT booting a sandbox. It:
//
//  1. parses the local snapshot.cfg + its lower chain;
//  2. validates every LOWER layer is remote, present, and sealed under the
//     current MANIFEST_KEY — manifest blob only, NO chunk download
//     (Config.CheckManifest). A lower file:// layer is rejected: the only local
//     layer permitted is the top (the local-layer invariant, docs §3.5) — the
//     caller must re-export it locally first to flatten it;
//  3. ingests THIS snapshot's own overlay + memory bundle into the store,
//     re-rendering snapshot.cfg so overlay.base points at the freshly-ingested
//     manifest:// overlay (the remote lower chain is carried by reference,
//     never re-uploaded);
//  4. returns the uploaded snapshot's identity = manifest://<memKey>.
//
// The result is a fully-remote snapshot (zero local layers) restorable via
// `sandbox-ctl run --restore=manifest://<memKey>`. Reuses the live snapshot
// path's IngestSink + BuildZIP, so an offline upload is byte-equivalent to a
// live --upload of the same content.
func UploadLocal(ctx context.Context, snapshotPath string, mcfg *manifest.Config, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	// 1. Trailing-ZIP members (config.json / state.json / snapshot.cfg).
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return "", fmt.Errorf("read snapshot ZIP trailer: %w", err)
	}
	ent := map[string][]byte{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return "", fmt.Errorf("zip open %s: %w", zf.Name, err)
		}
		b, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			return "", fmt.Errorf("zip read %s: %w", zf.Name, rerr)
		}
		ent[zf.Name] = b
	}
	for _, w := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := ent[w]; !ok {
			return "", fmt.Errorf("snapshot bundle missing %s (produced by old sandbox-ctl?)", w)
		}
	}
	parsed, err := ParseSnapshotCfg(ent["snapshot.cfg"])
	if err != nil {
		return "", err
	}

	// 2. The top disk layer must be a local file:// in the bundle dir. Single
	//    -disk records it at root.base; overlay at overlay.base.
	topDiskBase := parsed.Boot.Root.Base
	diskChain := parsed.Boot.Root.BaseFromRefs
	if !parsed.SingleDisk() {
		topDiskBase = parsed.Boot.Root.Overlay.Base
		diskChain = parsed.Boot.Root.Overlay.BaseFromRefs
	}
	ovScheme, ovVal, ok := config.SchemeAndPath(topDiskBase)
	if !ok || ovScheme != "file" {
		return "", fmt.Errorf("upload-snapshot: top disk layer %q is not a local file:// (already remote? nothing to upload)", topDiskBase)
	}
	overlayPath := ovVal
	if !filepath.IsAbs(overlayPath) {
		overlayPath = filepath.Join(filepath.Dir(snapshotPath), overlayPath)
	}

	// 3. Validate every LOWER layer (memory + disk chains): remote, present,
	//    key-consistent. Reject buried local layers (invariant).
	lower := append(append([]string{}, parsed.FromRefs...), diskChain...)
	for _, ref := range lower {
		sc, val, ok := config.SchemeAndPath(ref)
		if !ok {
			return "", fmt.Errorf("upload-snapshot: malformed lower ref %q", ref)
		}
		switch sc {
		case "file":
			return "", fmt.Errorf("upload-snapshot: lower layer %q is a local file:// — re-export locally to flatten it first (local-layer invariant, docs §3.5)", ref)
		case "manifest":
			for _, part := range strings.Split(val, ":") { // manifest://k1:k2 → per-key check
				key, perr := parseManifestKey(part)
				if perr != nil {
					return "", fmt.Errorf("upload-snapshot: lower ref %q: %w", ref, perr)
				}
				if cerr := mcfg.CheckManifest(ctx, key); cerr != nil {
					return "", fmt.Errorf("upload-snapshot: lower layer %q: %w", ref, cerr)
				}
			}
		default:
			return "", fmt.Errorf("upload-snapshot: lower ref %q: unknown scheme %q", ref, sc)
		}
	}
	logf("upload-snapshot: %d lower layer(s) verified present + key-consistent", len(lower))

	// 4. Ingest this snapshot's own overlay + memory bundle (reusing IngestSink).
	ing, err := mcfg.NewIngester(mcfg.IngestKeyFunc(), nil)
	if err != nil {
		return "", fmt.Errorf("ingester: %w", err)
	}
	defer ing.Close()
	sink := snapshot.NewIngestSink(ing, logf)

	ovf, err := os.Open(overlayPath)
	if err != nil {
		return "", fmt.Errorf("open overlay %s: %w", overlayPath, err)
	}
	defer ovf.Close()
	ovst, err := ovf.Stat()
	if err != nil {
		return "", err
	}
	ovHoles, err := snapshot.WalkHoles(int(ovf.Fd()), ovst.Size())
	if err != nil {
		return "", fmt.Errorf("overlay holes: %w", err)
	}
	overlayRef, _, err := sink.AbsorbOverlay(ctx, ovf, ovHoles)
	if err != nil {
		return "", fmt.Errorf("ingest overlay: %w", err)
	}

	// Re-render snapshot.cfg: the top disk layer → the ingested manifest:// ref;
	// the remote lower chain (from_refs / base_from_refs) carried by reference.
	if parsed.SingleDisk() {
		parsed.Boot.Root.Base = overlayRef
	} else {
		parsed.Boot.Root.Overlay.Base = overlayRef
	}
	newCfg, err := yaml.Marshal(&parsed)
	if err != nil {
		return "", fmt.Errorf("render snapshot.cfg: %w", err)
	}
	zipBytes, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  ent["config.json"],
		"state.json":   ent["state.json"],
		"snapshot.cfg": newCfg,
	})
	if err != nil {
		return "", fmt.Errorf("build zip: %w", err)
	}

	// Memory section = [0, capBytes); ingest [memory][new ZIP].
	capBytes, err := util.ParseSize(parsed.Resources.Capacity.Memory)
	if err != nil {
		return "", fmt.Errorf("capacity.memory: %w", err)
	}
	if int64(capBytes) > st.Size() {
		return "", fmt.Errorf("snapshot %s too small (%d) for its memory section (%d)", snapshotPath, st.Size(), capBytes)
	}
	memHoles, err := snapshot.WalkHoles(int(f.Fd()), int64(capBytes))
	if err != nil {
		return "", fmt.Errorf("memory holes: %w", err)
	}
	snapshotRef, _, err := sink.AbsorbBundle(ctx, io.NewSectionReader(f, 0, int64(capBytes)), memHoles, bytes.NewReader(zipBytes))
	if err != nil {
		return "", fmt.Errorf("ingest bundle: %w", err)
	}
	if ov, bu := sink.Results(); ov != nil && bu != nil {
		logf("upload-snapshot: overlay stored=%d dedup=%d; memory stored=%d dedup=%d",
			ov.StoredChunks, ov.DedupChunks, bu.StoredChunks, bu.DedupChunks)
	}
	return snapshotRef, nil
}

// parseManifestKey decodes a 64-hex manifest key into a store.ContentKey.
func parseManifestKey(hexKey string) (store.ContentKey, error) {
	var k store.ContentKey
	b, err := hex.DecodeString(hexKey)
	if err != nil {
		return k, fmt.Errorf("manifest key %q: %w", hexKey, err)
	}
	if len(b) != len(k) {
		return k, fmt.Errorf("manifest key %q: want %d bytes, got %d", hexKey, len(k), len(b))
	}
	copy(k[:], b)
	return k, nil
}
