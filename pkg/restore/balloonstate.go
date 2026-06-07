package restore

import (
	"encoding/json"
	"fmt"
)

// virtio-balloon PFN shift in CH (matches the linux uapi). Each balloon
// page is 1 << shift = 4096 bytes.
const balloonPFNShift = 12

// chSnapshotTree is the minimal structural shape of cloud-hypervisor's
// state.json that we need to extract the balloon device state. Real
// state.json carries many other branches we ignore.
type chSnapshotTree struct {
	Snapshots    map[string]*chSnapshotTree `json:"snapshots,omitempty"`
	SnapshotData *chSnapshotData            `json:"snapshot_data,omitempty"`
}

type chSnapshotData struct {
	// State is itself a JSON-encoded string per CH's vm-migration crate
	// (each device's state is double-encoded). For balloon, decoding it
	// yields balloonDeviceState below.
	State string `json:"state"`
}

type balloonDeviceState struct {
	Config balloonDeviceConfig `json:"config"`
}

type balloonDeviceConfig struct {
	// NumPages: pages host wants the guest to give up (the target).
	NumPages uint32 `json:"num_pages"`
	// Actual: pages the guest balloon driver has actually given up.
	Actual uint32 `json:"actual"`
}

// parseBalloonFromState walks state.json, finds the balloon device state
// at snapshots["device-manager"].snapshots["__balloon"], and returns its
// target_bytes / current_bytes.
//
// targetBytes  = num_pages * 4096       # what sandbox-ctl told CH via /vm.resize
// currentBytes = actual * 4096          # what guest balloon driver has reported
//
// Returns ok=false (with no error) if the bundle predates balloon use or
// the path is missing. Caller treats this as "no info, fall back to
// yaml.allocatable as cold-start equivalent". Returns an error only on
// malformed JSON.
func parseBalloonFromState(stateJSON []byte) (targetBytes, currentBytes uint64, ok bool, err error) {
	var tree chSnapshotTree
	if err := json.Unmarshal(stateJSON, &tree); err != nil {
		return 0, 0, false, fmt.Errorf("state.json: %w", err)
	}
	dm, ok2 := tree.Snapshots["device-manager"]
	if !ok2 || dm == nil {
		return 0, 0, false, nil
	}
	bal, ok2 := dm.Snapshots["__balloon"]
	if !ok2 || bal == nil || bal.SnapshotData == nil {
		return 0, 0, false, nil
	}
	var bs balloonDeviceState
	if err := json.Unmarshal([]byte(bal.SnapshotData.State), &bs); err != nil {
		return 0, 0, false, fmt.Errorf("balloon state inner: %w", err)
	}
	targetBytes = uint64(bs.Config.NumPages) << balloonPFNShift
	currentBytes = uint64(bs.Config.Actual) << balloonPFNShift
	return targetBytes, currentBytes, true, nil
}

// deriveAllocatableAtSnapshot computes the runtime memory budget the
// sandbox held at snapshot time, expressed as bytes.
//
//	allocatable_at_snapshot = capacity − min(balloon.target, balloon.current)
//
// The min() picks whichever balloon interpretation gives the LARGER
// allocatable, conservatively preserving guest's effective working set
// even when balloon target/current are still converging at snapshot time:
//   - inflating (current < target): pick current → guest still has the
//     larger memory until driver catches up
//   - deflating (target < current): pick target → host intends to give
//     guest the larger memory; driver hasn't expanded yet
//
// Returns capacity itself when balloon info is unavailable (ok=false),
// matching cold-start behaviour.
func deriveAllocatableAtSnapshot(capacityBytes, target, current uint64, ok bool) uint64 {
	if !ok {
		return capacityBytes
	}
	min := target
	if current < min {
		min = current
	}
	if min >= capacityBytes {
		return 0
	}
	return capacityBytes - min
}
