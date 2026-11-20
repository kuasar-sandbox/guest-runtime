package main

import (
	"os"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// runQuiesce executes the v1 pre-snapshot cleanup sequence
// (docs/guest-abi.md#quiesce):
//
//  1. syncfs(2) on pinned managed writable filesystems
//  2. optional drop_caches write — drop clean page + dentry/inode cache
//
// Filesystem errors abort capture. Cache cleanup remains a best-effort dedup
// aid, not a snapshot gate. External mounts never enter the managed registry.
//
// The /tmp tmpfs reset and the application-level signal hook described
// in docs/guest-abi.md#quiesce (quiesce extension items) are not implemented here.
func runQuiesce(skipDropCaches bool) (proto.DropCachesResult, error) {
	return runQuiesceWith(skipDropCaches, guestFilesystems.sync, writeFile)
}

func runQuiesceWith(skipDropCaches bool, syncFn func() error, writeFn func(string, []byte) error) (proto.DropCachesResult, error) {
	t0 := time.Now()

	if err := syncFn(); err != nil {
		return proto.DropCachesSkipped, err
	}
	tSync := time.Since(t0)

	tDrop := time.Duration(0)
	result := proto.DropCachesSkipped
	if skipDropCaches {
		logf("quiesce: drop_caches skipped by snapshot request")
	} else if err := writeFn("/proc/sys/vm/drop_caches", []byte("3\n")); err != nil {
		result = proto.DropCachesFailed
		logf("quiesce: drop_caches write failed: %v (continuing)", err)
	} else {
		result = proto.DropCachesSucceeded
		tDrop = time.Since(t0) - tSync
	}

	logf("quiesce: sync=%dµs drop_caches=%dµs result=%s total=%dµs",
		tSync.Microseconds(), tDrop.Microseconds(), result, time.Since(t0).Microseconds())
	return result, nil
}

func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
