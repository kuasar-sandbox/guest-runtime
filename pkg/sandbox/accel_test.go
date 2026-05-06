package sandbox

import (
	"crypto/rand"
	"testing"

	"github.com/fullof-work/mass-sandbox/pkg/crypto"
	"github.com/fullof-work/mass-sandbox/pkg/manifest"
)

// TestNeedsAccelRuntime — file:// only configs skip the accel dial;
// either disk URI being manifest:// flips the bit.
func TestNeedsAccelRuntime(t *testing.T) {
	cases := []struct {
		name string
		base string
		olay string
		want bool
	}{
		{"file_only", "file:///x.erofs", "file:///x.ext4", false},
		{"file_only_no_overlay", "file:///x.erofs", "", false},
		{"manifest_blk0", "manifest://abcd", "file:///x.ext4", true},
		{"manifest_overlay", "file:///x.erofs", "manifest://abcd", true},
		{"manifest_both", "manifest://aaaa", "manifest://bbbb", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SandboxConfig{}
			cfg.Boot.Root.Base = tc.base
			cfg.Boot.Root.Overlay.Base = tc.olay
			got := needsAccelRuntime(cfg)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnsealManifestKeys_RoundTrip — seal then unseal a small key
// table (1 zero + 2 non-zero entries) and verify keys come back at
// the right slots with the zero slot left as zero.
func TestUnsealManifestKeys_RoundTrip(t *testing.T) {
	var customer [32]byte
	if _, err := rand.Read(customer[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// Two non-zero keys for entries 0 and 2; entry 1 is a zero chunk.
	var k0, k2 [32]byte
	for i := range k0 {
		k0[i] = byte(i)
	}
	for i := range k2 {
		k2[i] = byte(0xC0 ^ i)
	}
	flat := make([]byte, 0, 64)
	flat = append(flat, k0[:]...)
	flat = append(flat, k2[:]...)

	m := &manifest.Manifest{
		Version:   manifest.Version1,
		ImageSize: 3 * 4096,
		Entries: []manifest.ChunkEntry{
			{Offset: 0, Size: 4096},
			{Offset: 4096, Size: 4096, IsZero: true},
			{Offset: 8192, Size: 4096},
		},
	}

	kte := &crypto.AESKeyTableEncryptor{}
	sealed, err := kte.Seal(customer, flat, manifest.BuildAAD(m))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	keys, err := unsealManifestKeys(m, sealed, customer, kte)
	if err != nil {
		t.Fatalf("unsealManifestKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("len(keys)=%d, want 3", len(keys))
	}
	if keys[0] != k0 {
		t.Errorf("keys[0] mismatch")
	}
	var zero [32]byte
	if keys[1] != zero {
		t.Errorf("keys[1] should be zero (entry IsZero)")
	}
	if keys[2] != k2 {
		t.Errorf("keys[2] mismatch")
	}
}

// TestUnsealManifestKeys_LengthMismatch — sealed table with the wrong
// number of bytes is rejected with a clear error.
func TestUnsealManifestKeys_LengthMismatch(t *testing.T) {
	var customer [32]byte
	m := &manifest.Manifest{
		Version:   manifest.Version1,
		ImageSize: 4096,
		Entries:   []manifest.ChunkEntry{{Offset: 0, Size: 4096}},
	}
	// Seal a 64-byte payload, but the manifest only has 1 non-zero entry
	// (expects 32 bytes).
	bogus := make([]byte, 64)
	kte := &crypto.AESKeyTableEncryptor{}
	sealed, err := kte.Seal(customer, bogus, manifest.BuildAAD(m))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := unsealManifestKeys(m, sealed, customer, kte); err == nil {
		t.Fatalf("expected error on length mismatch, got nil")
	}
}
