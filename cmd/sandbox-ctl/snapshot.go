package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fullof-work/mass-sandbox/pkg/sandbox/snapshot"
)

func snapshotCmd(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	sandboxID := fs.String("sandbox-id", "", "target sandbox id (required)")
	outDir := fs.String("output", "", "local output dir (required when --upload not given)")
	upload := fs.Bool("upload", false, "ingest disk + snapshot into manifest store; stdout = snapshot manifest key")
	resume := fs.Bool("resume", true, "resume the sandbox after snapshot")
	runtimeRoot := fs.String("runtime-root", "/run", "directory containing /<sid>/ctl.sock")
	timeoutS := fs.Int("timeout", 60, "seconds to wait for snapshot_done")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sandboxID == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --sandbox-id required")
		return 2
	}
	if !*upload && *outDir == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --output required when --upload not given")
		return 2
	}

	if *outDir != "" {
		abs, err := filepath.Abs(*outDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*outDir = abs
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	ctlSock := filepath.Join(*runtimeRoot, *sandboxID, "ctl.sock")
	c, err := net.DialTimeout("unix", ctlSock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: dial %s: %v\n", ctlSock, err)
		return 1
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Duration(*timeoutS) * time.Second))

	req := snapshot.Request{
		Type:        snapshot.TypeSnapshotRequest,
		OutDir:      *outDir,
		Upload:      *upload,
		ResumeAfter: *resume,
	}
	if err := snapshot.WriteMessage(c, &req); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: send request: %v\n", err)
		return 1
	}
	var resp snapshot.Response
	if err := snapshot.ReadMessage(c, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: recv response: %v\n", err)
		return 1
	}
	if resp.Type == snapshot.TypeError {
		fmt.Fprintf(os.Stderr, "snapshot: error from sandbox: %s\n", resp.Msg)
		return 1
	}
	if *upload {
		// Match `manifest-ctl store --put-manifest`: stdout = manifest key,
		// human-readable details to stderr. Lets `MK=$(sandbox-ctl snapshot
		// --upload ...)` work in shell pipelines.
		fmt.Fprintf(os.Stderr, "snapshot upload done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
			resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
		if resp.DiskManifestKey != "" {
			fmt.Fprintf(os.Stderr, "  disk manifest key: %s\n", resp.DiskManifestKey)
		}
		if resp.Msg != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", resp.Msg)
		}
		fmt.Println(resp.SnapshotManifestKey)
		return 0
	}

	fmt.Printf("snapshot done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
		resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
	if resp.SnapshotPath != "" {
		fmt.Printf("  sandbox.snapshot: %s\n", resp.SnapshotPath)
	}
	if resp.DiskPath != "" {
		fmt.Printf("  disk.ext4: %s\n", resp.DiskPath)
	}
	return 0
}
