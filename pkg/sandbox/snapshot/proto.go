// Package snapshot implements the sandbox-ctl snapshot path: ctl.sock
// IPC, CH /vm.snapshot orchestration, sparse memfd copy, ZIP-at-end
// bundle composition.
//
// See sandbox-design.md §6 for the file format and §6.2 for the
// timing.
package snapshot

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Wire format on /run/<sid>/ctl.sock — JSON over u32 LE length prefix,
// matching launch / va_report.

// Request is the snapshot request from sandbox-ctl snapshot to the
// running sandbox-ctl run.
type Request struct {
	Type         string `json:"type"`
	OutDir       string `json:"out_dir,omitempty"`        // local mode: write here
	Upload       bool   `json:"upload,omitempty"`         // ingest into manifest store
	ResumeAfter  bool   `json:"resume_after,omitempty"`
	StagingDir   string `json:"staging_dir,omitempty"`    // CH /vm.snapshot dest
}

// Response is the success reply from the run-process.
type Response struct {
	Type                 string `json:"type"`
	MemorySize           uint64 `json:"memory_size,omitempty"`
	MemoryResident       uint64 `json:"memory_resident,omitempty"`
	WallclockPauseMs     int64  `json:"wallclock_pause_ms,omitempty"`
	WallclockDumpMs      int64  `json:"wallclock_dump_ms,omitempty"`
	SnapshotManifestKey  string `json:"snapshot_manifest_key,omitempty"`
	DiskManifestKey      string `json:"disk_manifest_key,omitempty"`
	SnapshotPath         string `json:"snapshot_path,omitempty"`
	DiskPath             string `json:"disk_path,omitempty"`
	Msg                  string `json:"msg,omitempty"` // for type=error
}

const (
	TypeSnapshotRequest = "snapshot_request"
	TypeSnapshotDone    = "snapshot_done"
	TypeError           = "error"
)

// MaxMessageBytes caps any single message on ctl.sock.
const MaxMessageBytes = 64 * 1024

// WriteMessage writes v as length-prefixed JSON.
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("snapshot: message too large: %d > %d", len(body), MaxMessageBytes)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadMessage reads length-prefixed JSON into v (a pointer).
func ReadMessage(r io.Reader, v any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n > MaxMessageBytes {
		return fmt.Errorf("snapshot: oversized message: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
