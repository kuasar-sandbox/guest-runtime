package sandbox

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/cache"
	cacheclient "github.com/fullof-work/mass-sandbox/pkg/cache/client"
	"github.com/fullof-work/mass-sandbox/pkg/config"
	"github.com/fullof-work/mass-sandbox/pkg/crypto"
	storeclient "github.com/fullof-work/mass-sandbox/pkg/store/client"
)

// accelRuntime bundles the long-lived state required to resolve a
// manifest:// disk URI: store-ctl gRPC client, cache-ctl wire client,
// crypto encryptors, and the customer key. One instance is shared by
// every manifest:// disk in a single sandbox lifecycle.
//
// Open and Close are paired with the sandbox-ctl process — clients are
// dialled at sandbox start, freed at exit. Per-RPC timeouts come from
// AcceleratorConfig (Store.Timeout / Cache.Timeout); there is no
// separate dial timeout (gRPC dials are non-blocking and the first RPC
// surfaces unreachability via the per-RPC deadline).
type accelRuntime struct {
	store       *storeclient.Client
	cacheGetter cache.Getter
	cacheCloser io.Closer // closes either the wire client or the store client (when cache bypassed)
	chunkEnc    crypto.ChunkEncryptor
	ktEnc       crypto.KeyTableEncryptor
	customerKey [32]byte
}

// openAccelRuntime dials store-ctl + cache-ctl and constructs the
// crypto encryptors based on cfg. Mirrors the boot path used by
// manifest-ctl so the same accelerator.yaml drives both.
func openAccelRuntime(cfg *AcceleratorConfig) (*accelRuntime, error) {
	if cfg == nil {
		return nil, errors.New("accel: nil accelerator config (manifest:// disks require --accelerator-config)")
	}
	if cfg.Store.Endpoint == "" {
		return nil, errors.New("accel: store.endpoint is required for manifest:// disks")
	}

	storeTimeout := 5 * time.Second
	if cfg.Store.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Store.Timeout); err == nil && d > 0 {
			storeTimeout = d
		}
	}
	storePool := cfg.Store.Pool
	if storePool <= 0 {
		storePool = 4
	}
	sc, err := storeclient.New(cfg.Store.Endpoint, storePool, storeTimeout)
	if err != nil {
		return nil, fmt.Errorf("accel: dial store-ctl: %w", err)
	}

	// Cache: when an endpoint is configured, dial the wire client.
	// When empty, fall back to a direct store reader so the rest of
	// the path (Fetcher) doesn't need to know about the bypass.
	var (
		cacheGetter cache.Getter
		cacheCloser io.Closer
	)
	if cfg.Cache.Endpoint != "" {
		cacheTimeout := 2 * time.Second
		if cfg.Cache.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Cache.Timeout); err == nil && d > 0 {
				cacheTimeout = d
			}
		}
		cachePool := cfg.Cache.Pool
		if cachePool <= 0 {
			cachePool = 4
		}
		// BlobPool: sized to the largest expected ciphertext (CDC max
		// chunk + flag byte). Without this, every Get() allocates a
		// fresh ~512KB buffer (cache.DefaultPool == make()), driving
		// >100 MiB of GC pressure across one cold-start. The pool
		// returns oversize requests via fresh make(); steady state is
		// NumP buffers retained.
		blobPool := cache.NewPool(blobPoolSize(cfg))
		cc, err := cacheclient.NewGetter(cfg.Cache.Endpoint, cacheclient.Options{
			Pool:     cachePool,
			Timeout:  cacheTimeout,
			BlobPool: blobPool,
		})
		if err != nil {
			sc.Close()
			return nil, fmt.Errorf("accel: dial cache-ctl: %w", err)
		}
		cacheGetter = cc
		cacheCloser = cc
	} else {
		cacheGetter = cache.NewStoreOrigin(sc)
		cacheCloser = sc // shared with store; nil out to avoid double-close
	}

	chunkEnc, err := crypto.NewChunkEncryptor(cfg.Crypto.Chunk)
	if err != nil {
		_ = cacheCloser.Close()
		if cacheCloser != sc {
			_ = sc.Close()
		}
		return nil, fmt.Errorf("accel: chunk encryptor: %w", err)
	}
	ktEnc, err := crypto.NewKeyTableEncryptor(cfg.Crypto.Manifest)
	if err != nil {
		_ = cacheCloser.Close()
		if cacheCloser != sc {
			_ = sc.Close()
		}
		return nil, fmt.Errorf("accel: key-table encryptor: %w", err)
	}

	customerKey, err := cfg.CustomerKey()
	if err != nil {
		_ = cacheCloser.Close()
		if cacheCloser != sc {
			_ = sc.Close()
		}
		return nil, fmt.Errorf("accel: customer key: %w", err)
	}

	return &accelRuntime{
		store:       sc,
		cacheGetter: cacheGetter,
		cacheCloser: cacheCloser,
		chunkEnc:    chunkEnc,
		ktEnc:       ktEnc,
		customerKey: customerKey,
	}, nil
}

// Close releases the underlying connection pools. Safe to call once.
func (a *accelRuntime) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.cacheCloser != nil {
		if err := a.cacheCloser.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// When cache was bypassed, cacheCloser == store; don't double-close.
	if a.store != nil && a.cacheCloser != a.store {
		if err := a.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// blobPoolSize picks the pool buffer size from chunker config when
// available, falling back to 1 MiB (matches the default CDC max).
// Add a small slack for the encryptor flag byte and any future
// header growth.
func blobPoolSize(cfg *AcceleratorConfig) int {
	const fallback = 1 << 20 // 1 MiB
	if cfg == nil {
		return fallback
	}
	// Use Chunk.CDC.Max when set; ignore Fixed.Size since it's
	// usually smaller and a too-small pool just falls back to fresh
	// make(). +4 KiB slack for the flag byte / pad.
	if cfg.Chunk.CDC.Max != "" {
		if n, err := config.ParseSize(cfg.Chunk.CDC.Max); err == nil && n > 0 {
			return int(n) + 4096
		}
	}
	return fallback
}

// needsAccelRuntime returns true if any disk URI in cfg uses the
// manifest:// scheme. The result decides whether sandbox-ctl must
// dial store-ctl + cache-ctl on this run.
func needsAccelRuntime(cfg *SandboxConfig) bool {
	candidates := []string{
		cfg.Boot.Root.Base,
		cfg.Boot.Root.Overlay.Base,
	}
	for _, uri := range candidates {
		if uri == "" {
			continue
		}
		if scheme, _, ok := SchemeAndPath(uri); ok && scheme == "manifest" {
			return true
		}
	}
	return false
}
