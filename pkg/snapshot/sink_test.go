package snapshot

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/sandbox-accelerator/pkg/sparse"
)

// dataSegments must return exactly the resident runs = [0,size) minus holes.
func TestDataSegments(t *testing.T) {
	cases := []struct {
		name  string
		size  int64
		holes []sparse.Extent
		want  []sparse.Extent
	}{
		{"no holes", 100, nil, []sparse.Extent{{Offset: 0, Size: 100}}},
		{"leading hole", 100, []sparse.Extent{{Offset: 0, Size: 40}}, []sparse.Extent{{Offset: 40, Size: 60}}},
		{"trailing hole", 100, []sparse.Extent{{Offset: 60, Size: 40}}, []sparse.Extent{{Offset: 0, Size: 60}}},
		{"middle hole", 100, []sparse.Extent{{Offset: 40, Size: 20}}, []sparse.Extent{{Offset: 0, Size: 40}, {Offset: 60, Size: 40}}},
		{"all hole", 100, []sparse.Extent{{Offset: 0, Size: 100}}, nil},
		{"two holes", 100, []sparse.Extent{{Offset: 10, Size: 10}, {Offset: 50, Size: 10}},
			[]sparse.Extent{{Offset: 0, Size: 10}, {Offset: 20, Size: 30}, {Offset: 60, Size: 40}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dataSegments(tc.size, tc.holes); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dataSegments(%d, %v) = %v, want %v", tc.size, tc.holes, got, tc.want)
			}
		})
	}
}

// concatReadSeeker must present [mem][tail] as one seekable stream so
// ingest.Ingest can Seek to each data segment across the memory/ZIP boundary.
func TestConcatReadSeeker(t *testing.T) {
	mem := []byte("0123456789") // memSize = 10
	tail := []byte("ABCDEF")    // 6
	full := append(append([]byte{}, mem...), tail...)
	c := &concatReadSeeker{mem: bytes.NewReader(mem), memSize: int64(len(mem)), tail: tail}

	// (1) full sequential read reconstructs mem||tail (mem EOF must not stop it).
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("sequential = %q, want %q", got, full)
	}

	// (2) seek into the tail region, read to end.
	if _, err := c.Seek(12, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full[12:]) {
		t.Fatalf("tail read = %q, want %q", got, full[12:])
	}

	// (3) seek to mem, ReadFull a span straddling the boundary [8,13).
	if _, err := c.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	span := make([]byte, 5)
	if _, err := io.ReadFull(c, span); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(span, full[8:13]) {
		t.Fatalf("straddle = %q, want %q", span, full[8:13])
	}

	// (4) SeekEnd reports total length.
	if end, err := c.Seek(0, io.SeekEnd); err != nil || end != int64(len(full)) {
		t.Fatalf("SeekEnd = %d, %v; want %d, nil", end, err, len(full))
	}
}

// writeSparseFile must reproduce the logical content (holes read back as zeros,
// tail appended after size) AND leave the hole region unallocated on disk.
func TestWriteSparseFileRoundTrip(t *testing.T) {
	const size = 1 << 20 // 1 MiB
	src := make([]byte, size)
	copy(src[0:4], "HEAD")
	copy(src[size-4:], "TAIL")
	holes := []sparse.Extent{{Offset: 4, Size: size - 8}} // [4, size-4) is a hole
	tail := []byte("ZIPTRAILER")

	path := filepath.Join(t.TempDir(), "out.bin")
	if err := writeSparseFile(path, bytes.NewReader(src), size, holes, bytes.NewReader(tail)); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, src...), tail...)
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: len got=%d want=%d", len(got), len(want))
	}

	// Sparseness: only ~3 blocks of real data were written into 1 MiB, so the
	// allocated size must be far below the logical size (a dumb dense copy would
	// allocate the whole file).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := fi.Sys().(*syscall.Stat_t).Blocks * 512; allocated >= size {
		t.Errorf("not sparse: allocated=%d bytes, logical=%d", allocated, size)
	}
}

// BuildZIP must be byte-deterministic (sorted names, fixed mtime) and readable.
func TestBuildZIPDeterministic(t *testing.T) {
	entries := map[string][]byte{
		"snapshot.cfg": []byte("cfg"),
		"config.json":  []byte(`{"a":1}`),
		"state.json":   []byte("state"),
	}
	a, err := BuildZIP(entries)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildZIP(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("BuildZIP not deterministic across calls")
	}

	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(data)
	}
	for name, want := range map[string]string{"snapshot.cfg": "cfg", "config.json": `{"a":1}`, "state.json": "state"} {
		if got[name] != want {
			t.Errorf("entry %s = %q, want %q", name, got[name], want)
		}
	}
}
