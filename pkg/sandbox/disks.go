package sandbox

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/fullof-work/mass-sandbox/pkg/cache"
	"github.com/fullof-work/mass-sandbox/pkg/crypto"
	"github.com/fullof-work/mass-sandbox/pkg/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/store"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// openBlockReader resolves a file:// or manifest:// disk URI into a
// vhost.BlockReader plus the total disk size.
//
// For manifest:// the accel parameter must carry an opened
// accelRuntime (store + cache clients + crypto encryptors). Callers
// are responsible for opening accel ahead of time when any disk URI in
// the sandbox config uses manifest://.
//
// ctx scopes the lifetime of asynchronous chunk fetches kicked off by
// later ReadAt calls; cancelling it makes pending vhost-user-blk reads
// fail promptly during sandbox shutdown.
func openBlockReader(ctx context.Context, uri string, accel *accelRuntime) (vhost.BlockReader, int64, error) {
	scheme, value, ok := SchemeAndPath(uri)
	if !ok {
		return nil, 0, fmt.Errorf("invalid disk URI: %s", uri)
	}
	switch scheme {
	case "file":
		fr, err := vhost.OpenFileReader(value)
		if err != nil {
			return nil, 0, err
		}
		return fr, fr.Size(), nil
	case "manifest":
		if accel == nil {
			return nil, 0, errors.New("manifest:// disk URI requires accelerator config (run sandbox-ctl with --accelerator-config)")
		}
		return openManifestReader(ctx, value, accel)
	default:
		return nil, 0, fmt.Errorf("unknown disk URI scheme: %s", scheme)
	}
}

// openManifestReader fetches the manifest blob keyed by hex, decodes
// it, unseals the key table, and constructs a vhost.BlockReader backed
// by a fetch.Fetcher. The returned reader's underlying chunk fetches
// reuse the connection pools owned by accel — there is no per-reader
// dial.
func openManifestReader(ctx context.Context, hexKey string, accel *accelRuntime) (vhost.BlockReader, int64, error) {
	fetcher, size, err := openManifestFetcher(ctx, hexKey, accel)
	if err != nil {
		return nil, 0, err
	}
	return vhost.NewManifestReader(ctx, fetcher, size), size, nil
}

// OpenManifestFetcher dials a manifest:// resource and returns a
// fetch.Fetcher (random-access reader) over its content. Used by
// snapshot restore to read the ZIP at the end of a manifest://
// sandbox.snapshot bundle and to feed ManifestSnapshotSource for
// memory-page lazy loading.
//
// Exposed in capital case so cmd/sandbox-ctl/restore.go can invoke it
// without going through the per-disk wrapper.
func OpenManifestFetcher(ctx context.Context, hexKey string, accel *AccelRuntime) (*fetch.Fetcher, int64, error) {
	if accel == nil || accel.inner == nil {
		return nil, 0, errors.New("manifest:// requires accelerator runtime")
	}
	return openManifestFetcher(ctx, hexKey, accel.inner)
}

// AccelRuntime is a public wrapper around the per-sandbox accelerator
// runtime so commands outside pkg/sandbox (cmd/sandbox-ctl/restore.go)
// can pass it through without exposing the internal struct.
type AccelRuntime struct {
	inner *accelRuntime
}

// OpenAccelRuntime is the package-public constructor for AccelRuntime.
// Call once per sandbox-ctl restore invocation when --snapshot is a
// manifest:// reference.
func OpenAccelRuntime(cfg *AcceleratorConfig) (*AccelRuntime, error) {
	r, err := openAccelRuntime(cfg)
	if err != nil {
		return nil, err
	}
	return &AccelRuntime{inner: r}, nil
}

// Close releases the underlying connection pools.
func (a *AccelRuntime) Close() error {
	if a == nil || a.inner == nil {
		return nil
	}
	return a.inner.Close()
}

// openManifestFetcher is the unexported core that disks.go and the
// public restore helper share.
func openManifestFetcher(ctx context.Context, hexKey string, accel *accelRuntime) (*fetch.Fetcher, int64, error) {
	keyBytes, err := hex.DecodeString(hexKey)
	if err != nil || len(keyBytes) != 32 {
		return nil, 0, fmt.Errorf("manifest:// expects 32-byte hex content key, got %q", hexKey)
	}
	var key store.ContentKey
	copy(key[:], keyBytes)

	result, blob, err := accel.cacheGetter.Get(ctx, store.PartitionManifest, key)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch manifest blob: %w", err)
	}
	if result != cache.CacheHit {
		return nil, 0, fmt.Errorf("manifest blob not found: %s", hexKey)
	}
	mData := append([]byte(nil), blob.Bytes()...)
	blob.Release()

	m, sealedKT, err := manifest.Unmarshal(mData)
	if err != nil {
		return nil, 0, fmt.Errorf("unmarshal manifest: %w", err)
	}
	keys, err := unsealManifestKeys(m, sealedKT, accel.customerKey, accel.ktEnc)
	if err != nil {
		return nil, 0, err
	}
	fetcher := fetch.NewFetcher(m, keys, accel.cacheGetter, accel.chunkEnc)
	return fetcher, int64(m.ImageSize), nil
}

// unsealManifestKeys decrypts the sealed key table and expands it into
// a sparse N-length slice (N = chunk count). Zero entries get a zero
// key (never read by the fetch path). Mirrors manifest-ctl's helper of
// the same name so failure modes are consistent across CLIs.
func unsealManifestKeys(m *manifest.Manifest, sealedKT []byte, customerKey [32]byte, kte crypto.KeyTableEncryptor) ([][32]byte, error) {
	flat, err := kte.Unseal(customerKey, sealedKT, manifest.BuildAAD(m))
	if err != nil {
		return nil, fmt.Errorf("unseal key table: %w", err)
	}
	count := int(m.ChunkCount())
	nonZero := 0
	for _, e := range m.Entries {
		if !e.IsZero {
			nonZero++
		}
	}
	if len(flat) != nonZero*32 {
		return nil, fmt.Errorf("key table size mismatch: got %d bytes, want %d (non-zero chunks: %d)", len(flat), nonZero*32, nonZero)
	}
	keys := make([][32]byte, count)
	pos := 0
	for i, e := range m.Entries {
		if e.IsZero {
			continue
		}
		copy(keys[i][:], flat[pos*32:(pos+1)*32])
		pos++
	}
	return keys, nil
}
