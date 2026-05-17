package vhost

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
)

// BlockReader is the abstract source for read-only block data behind a
// vhost-user-blk backend. blk0 (base image) and blk1's optional base layer
// both go through this interface.
//
// ReadAt must be safe for concurrent use; the backend serves multiple
// virtq requests in parallel.
type BlockReader interface {
	ReadAt(buf []byte, offset int64) (int, error)
	Size() int64
	Close() error
}

// FileReader is a BlockReader backed by a local file via pread.
type FileReader struct {
	f    *os.File
	size int64
}

// OpenFileReader opens path for read-only and returns a FileReader.
func OpenFileReader(path string) (*FileReader, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("vhost: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vhost: stat %s: %w", path, err)
	}
	return &FileReader{f: f, size: st.Size()}, nil
}

func (r *FileReader) ReadAt(buf []byte, offset int64) (int, error) {
	return r.f.ReadAt(buf, offset)
}
func (r *FileReader) Size() int64    { return r.size }
func (r *FileReader) Close() error   { return r.f.Close() }

// ManifestReader is a BlockReader backed by a pkg/fetch.Fetcher resolving
// chunks via cache-ctl. Holes in the underlying manifest are transparently
// zero-filled — the guest sees a flat sparse block device.
type ManifestReader struct {
	fetcher fetch.Stream
	size    int64
	ctx     context.Context
}

// NewManifestReader wraps an already-opened fetch.Fetcher with the given
// total image size. The caller owns the Fetcher's lifecycle (typically
// the underlying cache/store clients live as long as the sandbox-ctl
// process).
func NewManifestReader(ctx context.Context, fetcher fetch.Stream, size int64) *ManifestReader {
	return &ManifestReader{fetcher: fetcher, size: size, ctx: ctx}
}

// ReadAt fills buf with bytes from the manifest's virtual image at
// offset. Hole regions are zero-filled inline by the Fetcher; callers
// of this BlockReader never see fetch.ErrHitHole.
//
// Satisfies io.ReaderAt: short reads at end-of-image come back with
// (n, io.EOF), past-EOF reads with (0, io.EOF). archive/zip and
// other io.ReaderAt consumers can treat ManifestReader the same as
// a file.
func (r *ManifestReader) ReadAt(buf []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, io.EOF
	}
	return r.fetcher.ReadAtBlock(r.ctx, buf, uint64(offset))
}

func (r *ManifestReader) Size() int64 { return r.size }

func (r *ManifestReader) Close() error {
	// Fetcher lifecycle is managed by the caller (typically tied to the
	// sandbox-ctl process); we don't own it.
	return nil
}
