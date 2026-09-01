// Package runtimebundle builds the host file passed directly to Cloud
// Hypervisor as a virtio-pmem backing. The raw EROFS remains at offset zero;
// an empty digest-marker ZIP is appended at EOF.
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
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

const pmemAlignment = int64(2 << 20)

var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Build writes inputPath as an offset-zero EROFS prefix, pads the prefix so
// the final file is 2 MiB aligned, appends a deterministic ZIP containing one
// empty digest marker, and atomically publishes outputPath. The digest covers
// the complete prefix (EROFS + padding), never the self-describing ZIP.
func Build(inputPath, outputPath string) (string, error) {
	in, err := os.Open(inputPath)
	if err != nil {
		return "", fmt.Errorf("open runtime EROFS: %w", err)
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("stat runtime EROFS: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("runtime EROFS %s is not a regular file", inputPath)
	}

	placeholderZIP, err := markerZIP(strings.Repeat("0", sha256.Size*2))
	if err != nil {
		return "", err
	}
	zipSize := int64(len(placeholderZIP))
	prefixSize := alignUp(st.Size()+zipSize, pmemAlignment) - zipSize
	paddingSize := prefixSize - st.Size()

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+".partial-")
	if err != nil {
		return "", fmt.Errorf("create runtime bundle: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	prefixWriter := io.MultiWriter(tmp, h)
	if _, err := io.Copy(prefixWriter, in); err != nil {
		return "", fmt.Errorf("copy runtime EROFS: %w", err)
	}
	if err := writeZeros(prefixWriter, paddingSize); err != nil {
		return "", fmt.Errorf("pad runtime bundle: %w", err)
	}
	hexDigest := fmt.Sprintf("%x", h.Sum(nil))
	digest := "digest:" + hexDigest
	footer, err := markerZIP(hexDigest)
	if err != nil {
		return "", err
	}
	if len(footer) != len(placeholderZIP) {
		return "", fmt.Errorf("runtime bundle ZIP size changed: placeholder=%d actual=%d", len(placeholderZIP), len(footer))
	}
	if _, err := tmp.Write(footer); err != nil {
		return "", fmt.Errorf("write runtime bundle ZIP: %w", err)
	}
	if final, err := tmp.Seek(0, io.SeekCurrent); err != nil {
		return "", err
	} else if final%pmemAlignment != 0 {
		return "", fmt.Errorf("runtime bundle size %d is not 2 MiB aligned", final)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync runtime bundle: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close runtime bundle: %w", err)
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return "", fmt.Errorf("publish runtime bundle: %w", err)
	}
	committed = true
	if dir, err := os.Open(filepath.Dir(outputPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return digest, nil
}

func markerZIP(hexDigest string) ([]byte, error) {
	if len(hexDigest) != sha256.Size*2 {
		return nil, fmt.Errorf("runtime bundle: invalid SHA256 length %d", len(hexDigest))
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{
		Name:     tarstream.DigestMarkerPrefix + hexDigest,
		Method:   zip.Store,
		Modified: zipEpoch,
	}
	hdr.SetMode(0o444)
	if _, err := zw.CreateHeader(hdr); err != nil {
		return nil, fmt.Errorf("create runtime digest marker: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close runtime marker ZIP: %w", err)
	}
	return buf.Bytes(), nil
}

func alignUp(n, alignment int64) int64 {
	return (n + alignment - 1) / alignment * alignment
}

func writeZeros(w io.Writer, n int64) error {
	buf := make([]byte, 1<<20)
	for n > 0 {
		chunk := int64(len(buf))
		if n < chunk {
			chunk = n
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}
