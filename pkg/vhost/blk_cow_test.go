package vhost

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// fakeReader is a BlockReader returning a fixed pattern.
type fakeReader struct {
	data []byte
}

func (r *fakeReader) ReadAt(buf []byte, offset int64) (int, error) {
	if offset >= int64(len(r.data)) {
		return 0, nil
	}
	end := offset + int64(len(buf))
	if end > int64(len(r.data)) {
		end = int64(len(r.data))
	}
	copy(buf, r.data[offset:end])
	return int(end - offset), nil
}
func (r *fakeReader) Size() int64  { return int64(len(r.data)) }
func (r *fakeReader) Close() error { return nil }

func TestBlockCOW_BareDiff_NoBaseRead(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	cow, err := OpenBlockCOW(diff, nil, 4*4096)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Read should return zeros (no base, no dirty data).
	buf := make([]byte, 4096)
	n, err := cow.ReadAt(buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4096 {
		t.Fatalf("ReadAt n=%d want 4096", n)
	}
	if !bytes.Equal(buf, make([]byte, 4096)) {
		t.Fatalf("expected zeros, got %x...", buf[:8])
	}
	if cow.DirtyCount() != 0 {
		t.Errorf("expected 0 dirty blocks, got %d", cow.DirtyCount())
	}
}

func TestBlockCOW_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	base := &fakeReader{data: bytes.Repeat([]byte("BASE"), 4096)} // 16 KiB
	cow, err := OpenBlockCOW(diff, base, 4*4096)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Write 4 KiB of 'X' at offset 4096 (block 1).
	payload := bytes.Repeat([]byte{'X'}, 4096)
	n, err := cow.WriteAt(payload, 4096)
	if err != nil || n != 4096 {
		t.Fatalf("WriteAt: n=%d err=%v", n, err)
	}
	if !cow.blockDirty(1) {
		t.Errorf("block 1 should be dirty after write")
	}
	if cow.blockDirty(0) || cow.blockDirty(2) {
		t.Errorf("only block 1 should be dirty")
	}

	// Read block 1 back: should get our X's.
	buf := make([]byte, 4096)
	if _, err := cow.ReadAt(buf, 4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("dirty block read mismatch")
	}

	// Read block 0: should get base data.
	if _, err := cow.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, []byte("BASEBASEBASE")) {
		t.Fatalf("clean block should fall through to base, got %q", buf[:12])
	}
}

func TestBlockCOW_BitmapRebuildFromExistingDiff(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	const size = 8 * 4096

	// Pre-create a sparse diff with data at block 2 only.
	f, err := os.Create(diff)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'A'}, 4096)
	if _, err := f.WriteAt(payload, 2*4096); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cow, err := OpenBlockCOW(diff, nil, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	if !cow.blockDirty(2) {
		t.Errorf("rebuilt bitmap should mark block 2 dirty")
	}
	if cow.blockDirty(0) || cow.blockDirty(1) || cow.blockDirty(3) {
		t.Errorf("rebuilt bitmap marked extra blocks dirty")
	}
	// Verify read returns the data we pre-wrote.
	buf := make([]byte, 4096)
	if _, err := cow.ReadAt(buf, 2*4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("data mismatch after rebuild")
	}
}

func TestBlockCOW_DeclaredSizeAlignment(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	_, err := OpenBlockCOW(diff, nil, 4097) // not 4K aligned
	if err == nil {
		t.Fatal("expected alignment error")
	}
}

func TestBlockCOW_ReadAcrossBlocks(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	const size = 4 * 4096
	cow, err := OpenBlockCOW(diff, nil, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Write 8 KiB of 'P' starting at offset 0 (covers block 0 and 1).
	if _, err := cow.WriteAt(bytes.Repeat([]byte{'P'}, 8192), 0); err != nil {
		t.Fatal(err)
	}
	// Read 16 KiB starting at 0: should be 8 KiB of P then 8 KiB of zeros.
	buf := make([]byte, 16*1024)
	if _, err := cow.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	expected := append(bytes.Repeat([]byte{'P'}, 8192), make([]byte, 8192)...)
	if !bytes.Equal(buf, expected) {
		t.Fatalf("multi-block read mismatch")
	}
}
