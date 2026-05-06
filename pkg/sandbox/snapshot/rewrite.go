package snapshot

import (
	"archive/zip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/fullof-work/mass-sandbox/pkg/store"
)

// rewriteSandboxCfgInBundle patches the trailing-ZIP entry sandbox.cfg
// inside a sandbox.snapshot bundle so its boot.root.overlay.base
// reads `manifest://<diskKey>`. The memory section is left untouched
// (sparse layout preserved). The ZIP is rewritten in place: the file
// is truncated to the memory-section size, then AppendZIP is called
// with the same entries (config.json, state.json, sandbox.cfg) plus
// the patched sandbox.cfg.
//
// Memory-section size is recovered from the ZIP central directory:
// archive/zip's BaseOffset (set when there's a prefix before the
// archive) gives the offset where the ZIP starts, which is exactly
// the memfd size that produced the bundle.
func rewriteSandboxCfgInBundle(path string, diskKey store.ContentKey) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	totalSize := st.Size()

	r, err := zip.NewReader(f, totalSize)
	if err != nil {
		return fmt.Errorf("zip read: %w", err)
	}

	// Capture entries verbatim except sandbox.cfg, which we patch.
	entries := make(map[string][]byte, len(r.File))
	for _, file := range r.File {
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("zip open %s: %w", file.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("zip read %s: %w", file.Name, err)
		}
		entries[file.Name] = body
	}

	cfgBody, ok := entries["sandbox.cfg"]
	if !ok {
		return fmt.Errorf("bundle missing sandbox.cfg")
	}
	patched, err := patchSandboxCfg(cfgBody, diskKey)
	if err != nil {
		return fmt.Errorf("patch sandbox.cfg: %w", err)
	}
	entries["sandbox.cfg"] = patched

	// Locate ZIP base = first local file header offset = memory-section size.
	zipBase, err := zipBaseOffsetFromFile(f, totalSize)
	if err != nil {
		return fmt.Errorf("locate zip base: %w", err)
	}
	if zipBase < 0 || zipBase > totalSize {
		return fmt.Errorf("invalid zip base offset %d (totalSize=%d)", zipBase, totalSize)
	}
	_ = r // keep variable in scope; entries already extracted

	// Truncate the bundle to memory-section size, then re-write the ZIP.
	if err := f.Truncate(zipBase); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := AppendZIP(f, zipBase, entries); err != nil {
		return fmt.Errorf("append zip: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// patchSandboxCfg rewrites the JSON's boot.root.overlay.base field to
// `manifest://<hex-disk-key>`. Round-trips through map[string]any so
// extra fields the rest of the system might add stay intact.
func patchSandboxCfg(body []byte, diskKey store.ContentKey) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	boot, _ := obj["boot"].(map[string]any)
	if boot == nil {
		boot = map[string]any{}
		obj["boot"] = boot
	}
	root, _ := boot["root"].(map[string]any)
	if root == nil {
		root = map[string]any{}
		boot["root"] = root
	}
	overlay, _ := root["overlay"].(map[string]any)
	if overlay == nil {
		overlay = map[string]any{}
		root["overlay"] = overlay
	}
	overlay["base"] = "manifest://" + HexKey(diskKey)
	return json.Marshal(obj)
}

// zipBaseOffsetFromFile scans the file's tail for the ZIP End-of-
// Central-Directory record (EOCD, signature 0x06054b50) and computes
// the byte offset where the ZIP archive begins. archive/zip's Reader
// keeps the equivalent (`baseOffset`) unexported, so we derive it
// ourselves.
//
// Layout invariants:
//
//	[..memory section..][..ZIP body..][..central dir..][EOCD]
//
// EOCD record fields used:
//
//	+12: u32 size_of_central_dir
//	+16: u32 offset_of_central_dir (relative to start of ZIP)
//
// So:
//
//	zipBase = eocd_pos_in_file - size_of_central_dir - offset_of_central_dir
//
// EOCD itself is ≥22 bytes; the comment field (≤64 KiB) trails it. We
// scan the last 64 KiB + 22 bytes for the signature.
func zipBaseOffsetFromFile(f *os.File, totalSize int64) (int64, error) {
	const eocdSig uint32 = 0x06054b50
	const minEOCDSize = 22
	const maxCommentLen = 65535

	scanLen := int64(maxCommentLen + minEOCDSize)
	if scanLen > totalSize {
		scanLen = totalSize
	}
	scanStart := totalSize - scanLen
	buf := make([]byte, scanLen)
	if _, err := f.ReadAt(buf, scanStart); err != nil && err != io.EOF {
		return 0, err
	}
	// Scan from the end for the signature.
	eocdRel := int64(-1)
	for i := len(buf) - minEOCDSize; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:i+4]) == eocdSig {
			// Ensure comment_len is consistent with this position
			// (commentLen + 22 + i == len(buf) means EOCD ends at buf
			// end and matches comment trailer).
			commentLen := int(binary.LittleEndian.Uint16(buf[i+20 : i+22]))
			if i+minEOCDSize+commentLen <= len(buf) {
				eocdRel = int64(i)
				break
			}
		}
	}
	if eocdRel < 0 {
		return 0, fmt.Errorf("EOCD record not found in tail %d bytes", scanLen)
	}
	eocd := buf[eocdRel : eocdRel+minEOCDSize]
	sizeCD := int64(binary.LittleEndian.Uint32(eocd[12:16]))
	offsetCD := int64(binary.LittleEndian.Uint32(eocd[16:20]))
	eocdAbs := scanStart + eocdRel
	zipBase := eocdAbs - sizeCD - offsetCD
	return zipBase, nil
}

// keep zip import in scope; archive/zip is used by callers above.
var _ = zip.NewReader
