package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/fullof-work/mass-sandbox/pkg/cache"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/crypto"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/codec"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/uffd"
	"github.com/fullof-work/mass-sandbox/pkg/store"
)

const tPage = uffd.PageSize

// fixedHashGetter returns a fixed plaintext for a single known hash;
// any other hash misses. Used to validate the source only fetches
// chunks within the run boundary.
type fixedHashGetter struct {
	hash  store.ContentKey
	plain []byte
	hits  int
}

func (g *fixedHashGetter) Get(_ context.Context, _ store.Partition, key store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	if key == g.hash {
		g.hits++
		return cache.CacheHit, cache.NewMemBlob(g.plain), nil
	}
	return cache.CacheMiss, nil, nil
}

// passEnc returns its captured plaintext on Decrypt regardless of key.
type passEnc struct{ plain []byte }

func (e *passEnc) Encrypt(_ [32]byte, p []byte) ([]byte, [32]byte)        { return p, [32]byte{} }
func (e *passEnc) Decrypt(_ [32]byte, _ []byte) ([]byte, error)           { return e.plain, nil }
func (e *passEnc) DecryptInPlace(_ [32]byte, _ []byte) ([]byte, error)    { return e.plain, nil }

// buildManifest constructs an in-memory manifest with the given
// (offset, size, isZero) entries and holes. Used to drive
// ManifestSnapshotSource without going through ingest.
func buildManifest(t *testing.T, imageSize uint64, entries []codec.ChunkEntry, holes []codec.HoleExtent) *codec.Manifest {
	t.Helper()
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: imageSize,
		Entries:   entries,
		Holes:     holes,
	}
	if err := m.ValidateGeometry(); err != nil {
		t.Fatalf("invalid manifest geometry: %v", err)
	}
	return m
}

// TestManifestSnapshotSource_DataCapAtChunkBoundary — adjacent data
// chunks must NOT merge into a single run. The handler asks for a
// huge buffer; ReadAt returns at most one chunk's worth.
func TestManifestSnapshotSource_DataCapAtChunkBoundary(t *testing.T) {
	const chunkSize = 4 * tPage // 16 KiB per chunk
	plain := bytes.Repeat([]byte{0xAB}, chunkSize)
	m := buildManifest(t, 3*chunkSize, []codec.ChunkEntry{
		{Offset: 0, Size: chunkSize, CiphertextHash: store.ContentKey{0xA1}},
		{Offset: chunkSize, Size: chunkSize, CiphertextHash: store.ContentKey{0xA2}},
		{Offset: 2 * chunkSize, Size: chunkSize, CiphertextHash: store.ContentKey{0xA3}},
	}, nil)

	getter := &fixedHashGetter{hash: store.ContentKey{0xA1}, plain: plain}
	f := fetch.NewStream(m, make([][32]byte, 3), getter, &passEnc{plain: plain})
	src := NewManifestSnapshotSource(context.Background(), f)

	buf := make([]byte, 8*tPage) // ask for two chunks worth
	n, zero, err := src.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if zero {
		t.Fatalf("expected non-zero classification for data chunk")
	}
	if n != chunkSize {
		t.Fatalf("n=%d, want %d (cap at chunk boundary, not %d)", n, chunkSize, len(buf))
	}
	for i := 0; i < n; i++ {
		if buf[i] != 0xAB {
			t.Fatalf("buf[%d]=%x, want 0xAB", i, buf[i])
		}
	}
	if getter.hits != 1 {
		t.Errorf("getter hits=%d, want 1 (one chunk fetched)", getter.hits)
	}
}

// TestManifestSnapshotSource_ZeroRunSpansHolesAndZeroChunks —
// adjacent holes and IsZero chunks merge into a single zero run
// since they all resolve to UFFDIO_ZEROPAGE without source I/O.
func TestManifestSnapshotSource_ZeroRunSpansHolesAndZeroChunks(t *testing.T) {
	// Layout:
	//   [0, chunk):       data chunk (non-zero)
	//   [chunk, 3*chunk): hole (2 chunks worth)
	//   [3*chunk, 4*chunk): zero chunk
	//   [4*chunk, 5*chunk): data chunk (non-zero)
	const chunkSize = 4 * tPage
	plain := bytes.Repeat([]byte{0xCC}, chunkSize)
	m := buildManifest(t, 5*chunkSize, []codec.ChunkEntry{
		{Offset: 0, Size: chunkSize, CiphertextHash: store.ContentKey{0xB1}},
		{Offset: 3 * chunkSize, Size: chunkSize, IsZero: true},
		{Offset: 4 * chunkSize, Size: chunkSize, CiphertextHash: store.ContentKey{0xB2}},
	}, []codec.HoleExtent{
		{Offset: chunkSize, Size: 2 * chunkSize},
	})

	getter := &fixedHashGetter{}
	f := fetch.NewStream(m, make([][32]byte, 3), getter, &passEnc{plain: plain})
	src := NewManifestSnapshotSource(context.Background(), f)

	// Read starting at the hole — expect the run to extend through
	// the hole AND the adjacent zero chunk (3 chunks of zeros).
	// Buf must accommodate the full merged run.
	buf := make([]byte, 4*chunkSize)
	n, zero, err := src.ReadAt(buf, chunkSize)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !zero {
		t.Fatalf("expected zero classification at hole offset")
	}
	wantBytes := 3 * chunkSize // hole(2*chunk) + zero chunk(1*chunk)
	if n != wantBytes {
		t.Fatalf("n=%d, want %d (hole+zero-chunk run)", n, wantBytes)
	}
	if getter.hits != 0 {
		t.Errorf("getter hits=%d, want 0 (zero run must not touch store)", getter.hits)
	}
}

// TestManifestSnapshotSource_HeadInsideHole — offset partway into a
// hole still classifies as zero and reports the run remaining.
func TestManifestSnapshotSource_HeadInsideHole(t *testing.T) {
	const chunkSize = 4 * tPage
	plain := bytes.Repeat([]byte{0xDD}, chunkSize)
	m := buildManifest(t, 3*chunkSize, []codec.ChunkEntry{
		{Offset: 0, Size: chunkSize, CiphertextHash: store.ContentKey{0xC1}},
		{Offset: 2 * chunkSize, Size: chunkSize, CiphertextHash: store.ContentKey{0xC2}},
	}, []codec.HoleExtent{
		{Offset: chunkSize, Size: chunkSize},
	})

	getter := &fixedHashGetter{}
	f := fetch.NewStream(m, make([][32]byte, 2), getter, &passEnc{plain: plain})
	src := NewManifestSnapshotSource(context.Background(), f)

	// Offset chunkSize+1*PageSize: 1 page into the hole.
	buf := make([]byte, 8*tPage)
	n, zero, err := src.ReadAt(buf, chunkSize+tPage)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !zero {
		t.Fatalf("expected zero classification inside hole")
	}
	// Hole extends from chunkSize to 2*chunkSize; we asked at chunkSize+tPage.
	wantBytes := chunkSize - tPage
	if uint64(n) != uint64(wantBytes) {
		t.Fatalf("n=%d, want %d (remainder of hole)", n, wantBytes)
	}
}

// TestManifestSnapshotSource_EOF — past-image-size reads return
// (0, true, io.EOF) so the handler doesn't loop.
func TestManifestSnapshotSource_EOF(t *testing.T) {
	const chunkSize = 4 * tPage
	m := buildManifest(t, chunkSize, []codec.ChunkEntry{
		{Offset: 0, Size: chunkSize, CiphertextHash: store.ContentKey{0xE1}},
	}, nil)
	f := fetch.NewStream(m, make([][32]byte, 1), &fixedHashGetter{}, &passEnc{})
	src := NewManifestSnapshotSource(context.Background(), f)

	buf := make([]byte, tPage)
	n, _, err := src.ReadAt(buf, chunkSize)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want io.EOF", err)
	}
	if n != 0 {
		t.Errorf("n=%d, want 0 at EOF", n)
	}
}

// TestManifestSnapshotSource_BufSmallerThanChunk — caller-side cap
// (state run shorter than a chunk) is respected.
func TestManifestSnapshotSource_BufSmallerThanChunk(t *testing.T) {
	const chunkSize = 8 * tPage
	plain := bytes.Repeat([]byte{0xEE}, chunkSize)
	m := buildManifest(t, chunkSize, []codec.ChunkEntry{
		{Offset: 0, Size: chunkSize, CiphertextHash: store.ContentKey{0xF1}},
	}, nil)
	getter := &fixedHashGetter{hash: store.ContentKey{0xF1}, plain: plain}
	f := fetch.NewStream(m, make([][32]byte, 1), getter, &passEnc{plain: plain})
	src := NewManifestSnapshotSource(context.Background(), f)

	buf := make([]byte, 3*tPage) // smaller than chunk
	n, zero, err := src.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if zero {
		t.Fatalf("expected non-zero")
	}
	if n != 3*tPage {
		t.Fatalf("n=%d, want %d (capped by len(buf))", n, 3*tPage)
	}
	for i := 0; i < n; i++ {
		if buf[i] != 0xEE {
			t.Fatalf("buf[%d]=%x, want 0xEE", i, buf[i])
		}
	}
}

// passthrough encryptor
var _ crypto.ChunkEncryptor = (*passEnc)(nil)
