package snapshot

import (
	"archive/zip"
	"bytes"
	"fmt"
	"sort"
)

// BuildZIP returns an in-memory ZIP archive of the given entries, written in
// sorted name order so identical content yields identical bytes (deterministic
// content-addressing). The snapshot bundle appends these bytes after the memory
// section; restore locates them via the trailing EOCD (archive/zip scans from
// EOF), so the memory-prefix size doesn't matter for correctness.
func BuildZIP(entries map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			return nil, fmt.Errorf("zip.Create %s: %w", name, err)
		}
		if _, err := f.Write(entries[name]); err != nil {
			return nil, fmt.Errorf("zip.Write %s: %w", name, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("zip.Close: %w", err)
	}
	return buf.Bytes(), nil
}
