package snapshot

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
)

// AppendZIP writes a ZIP archive containing the given entries to
// outFile starting at offset. The file is opened append-mode at the
// given offset (callers usually pre-truncate to that offset for the
// memory section). Returns the byte count of the ZIP section written.
func AppendZIP(outFile *os.File, offset int64, entries map[string][]byte) (int64, error) {
	if _, err := outFile.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek offset=%d: %w", offset, err)
	}
	w := zip.NewWriter(outFile)
	for name, body := range entries {
		f, err := w.Create(name)
		if err != nil {
			return 0, fmt.Errorf("zip.Create %s: %w", name, err)
		}
		if _, err := f.Write(body); err != nil {
			return 0, fmt.Errorf("zip.Write %s: %w", name, err)
		}
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("zip.Close: %w", err)
	}
	end, err := outFile.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return end - offset, nil
}

// ReadZIP opens a snapshot bundle (memory + ZIP at end) and returns
// the entries from the trailing ZIP. Requires the caller to know the
// memory section size (= the ZIP starts at memSize). archive/zip
// auto-locates EOCD from EOF, so the prefix size doesn't matter for
// correctness.
func ReadZIP(file *os.File, totalSize int64) (map[string][]byte, error) {
	r, err := zip.NewReader(file, totalSize)
	if err != nil {
		return nil, fmt.Errorf("zip.NewReader: %w", err)
	}
	out := make(map[string][]byte, len(r.File))
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}
		out[f.Name] = body
	}
	return out, nil
}
