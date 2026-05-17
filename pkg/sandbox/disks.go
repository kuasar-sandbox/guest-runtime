package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/fullof-work/mass-sandbox/pkg/manifest"
	"github.com/fullof-work/mass-sandbox/pkg/manifest/fetch"
	"github.com/fullof-work/mass-sandbox/pkg/store"
	"github.com/fullof-work/mass-sandbox/pkg/vhost"
)

// openBlockReader resolves a file:// or manifest:// disk URI into a
// vhost.BlockReader plus the total disk size.
//
// For manifest:// the fetcher parameter must be non-nil — it carries
// the cache-ctl / store-ctl client and the per-process decryptor.
// Cold-start lifecycle and restore both construct the fetcher up front
// (only when manifest:// resources are referenced) and share it across
// every disk URI.
//
// ctx scopes the lifetime of asynchronous chunk fetches kicked off by
// later ReadAt calls; cancelling it makes pending vhost-user-blk reads
// fail promptly during sandbox shutdown.
func openBlockReader(ctx context.Context, uri string, fetcher fetch.Fetcher) (vhost.BlockReader, int64, error) {
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
		if fetcher == nil {
			return nil, 0, errors.New("manifest:// disk URI requires manifest config (run sandbox-ctl with --manifest-config or set MANIFEST_CONFIG)")
		}
		stream, size, err := OpenManifestStream(ctx, value, fetcher)
		if err != nil {
			return nil, 0, err
		}
		return vhost.NewManifestReader(ctx, stream, size), size, nil
	default:
		return nil, 0, fmt.Errorf("unknown disk URI scheme: %s", scheme)
	}
}

// OpenManifestStream resolves a manifest:// hex content key into a
// fetch.Stream and its image size. Exported so callers outside this
// package (notably pkg/sandbox/restore for snapshot bundles) can share
// the same code path.
//
// fetcher's underlying store/cache client is shared with every read it
// produces; callers are responsible for closing it when the sandbox
// lifecycle ends.
func OpenManifestStream(ctx context.Context, hexKey string, fetcher fetch.Fetcher) (fetch.Stream, int64, error) {
	if fetcher == nil {
		return nil, 0, errors.New("manifest:// requires a fetch.Fetcher")
	}
	key, err := manifest.ParseHexKey(hexKey)
	if err != nil {
		return nil, 0, err
	}
	stream, err := fetcher.Fetch(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return stream, int64(stream.ImageSize()), nil
}

// needsManifestFetcher returns true if any disk URI in cfg uses the
// manifest:// scheme. The result decides whether sandbox-ctl must
// dial store-ctl / cache-ctl on this run.
func needsManifestFetcher(cfg *SandboxConfig) bool {
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

// Compile-time guard: store.ContentKey is used indirectly by
// ParseHexKey above. Keep the import alive without an unused symbol.
var _ = store.PartitionChunk
