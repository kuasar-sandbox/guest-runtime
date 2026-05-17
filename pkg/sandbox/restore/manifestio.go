package restore

import (
	"context"
	"io"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
)

// fetcherReaderAt wraps a fetch.Fetcher as an io.ReaderAt. archive/zip's
// NewReader needs random access to scan EOCD from the tail and to read
// each entry's local file header + data. The Fetcher already provides
// efficient block-aligned reads (one or more chunks via cache-ctl);
// archive/zip's small reads (~22 bytes for EOCD, plus per-entry
// headers) all hit the same chunks so cache-ctl dedup is good.
//
// Holes are zero-filled per fetch.Fetcher.ReadAtBlock semantics.
type fetcherReaderAt struct {
	ctx     context.Context
	fetcher fetch.Stream
	size    int64
}

func (f *fetcherReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n, err := f.fetcher.ReadAtBlock(f.ctx, p, uint64(off))
	return n, err
}
