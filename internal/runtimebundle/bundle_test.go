package runtimebundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "runtime.erofs")
	prefix := bytes.Repeat([]byte("erofs"), 12345)
	if err := os.WriteFile(raw, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(dir, "runtime-1.bundle")
	two := filepath.Join(dir, "runtime-2.bundle")
	digest, err := Build(raw, one)
	if err != nil {
		t.Fatal(err)
	}
	digest2, err := Build(raw, two)
	if err != nil {
		t.Fatal(err)
	}
	if digest2 != digest {
		t.Fatalf("digest changed: %q != %q", digest2, digest)
	}
	b1, err := os.ReadFile(one)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(two)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("runtime bundle is not deterministic")
	}
	if int64(len(b1))%pmemAlignment != 0 {
		t.Fatalf("bundle size %d is not aligned", len(b1))
	}
	if !bytes.Equal(b1[:len(prefix)], prefix) {
		t.Fatal("raw EROFS prefix changed")
	}

	placeholder, err := markerZIP(strings.Repeat("0", sha256.Size*2))
	if err != nil {
		t.Fatal(err)
	}
	prefixSize := len(b1) - len(placeholder)
	sum := sha256.Sum256(b1[:prefixSize])
	want := fmt.Sprintf("digest:%x", sum[:])
	if digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}

	zr, err := zip.NewReader(bytes.NewReader(b1), int64(len(b1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("ZIP entries = %d, want 1", len(zr.File))
	}
	zf := zr.File[0]
	if zf.Name != tarstream.DigestMarkerPrefix+strings.TrimPrefix(digest, "digest:") || zf.UncompressedSize64 != 0 {
		t.Fatalf("marker = %q/%d", zf.Name, zf.UncompressedSize64)
	}
	r, err := zf.Open()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, r); err != nil || n != 0 {
		t.Fatalf("empty marker read = %d, %v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
