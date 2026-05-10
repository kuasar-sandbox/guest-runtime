package restore

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildState constructs a synthetic state.json mimicking CH's tree
// structure with a balloon device entry.
func buildState(numPages, actual uint32) []byte {
	inner := map[string]any{
		"avail_features": 0,
		"acked_features": 0,
		"config": map[string]any{
			"num_pages": numPages,
			"actual":    actual,
		},
	}
	innerJSON, _ := json.Marshal(inner)
	tree := map[string]any{
		"snapshots": map[string]any{
			"device-manager": map[string]any{
				"snapshots": map[string]any{
					"__balloon": map[string]any{
						"snapshot_data": map[string]any{
							"state": string(innerJSON),
						},
					},
				},
			},
		},
	}
	out, _ := json.Marshal(tree)
	return out
}

func TestParseBalloonFromState(t *testing.T) {
	// 64 MiB target, 32 MiB current
	pagesTarget := uint32((64 << 20) >> balloonPFNShift)
	pagesActual := uint32((32 << 20) >> balloonPFNShift)
	state := buildState(pagesTarget, pagesActual)

	target, current, ok, err := parseBalloonFromState(state)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if target != 64<<20 {
		t.Errorf("target: want=%d got=%d", 64<<20, target)
	}
	if current != 32<<20 {
		t.Errorf("current: want=%d got=%d", 32<<20, current)
	}
}

func TestParseBalloonFromState_MissingTree(t *testing.T) {
	// state.json without device-manager / __balloon — older bundles or
	// balloon-disabled VMs.
	state := []byte(`{"snapshots":{}}`)
	_, _, ok, err := parseBalloonFromState(state)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false")
	}
}

func TestParseBalloonFromState_Malformed(t *testing.T) {
	_, _, _, err := parseBalloonFromState([]byte("not json"))
	if err == nil {
		t.Fatalf("expected error on malformed json")
	}
	if !strings.Contains(err.Error(), "state.json") {
		t.Errorf("error should mention state.json: %v", err)
	}
}

func TestDeriveAllocatableAtSnapshot_MinPicksConservative(t *testing.T) {
	cap := uint64(1024 << 20)

	// Inflating: target=200 MiB, current=100 MiB → min=100 MiB
	// allocatable should be capacity - 100 MiB (the larger value).
	got := deriveAllocatableAtSnapshot(cap, 200<<20, 100<<20, true)
	want := cap - 100<<20
	if got != want {
		t.Errorf("inflating: want=%d got=%d", want, got)
	}

	// Deflating: target=100 MiB, current=200 MiB → min=100 MiB
	got = deriveAllocatableAtSnapshot(cap, 100<<20, 200<<20, true)
	want = cap - 100<<20
	if got != want {
		t.Errorf("deflating: want=%d got=%d", want, got)
	}

	// Balloon stable: target=current=128 MiB → min=128 MiB
	got = deriveAllocatableAtSnapshot(cap, 128<<20, 128<<20, true)
	want = cap - 128<<20
	if got != want {
		t.Errorf("stable: want=%d got=%d", want, got)
	}
}

func TestDeriveAllocatableAtSnapshot_NoBalloonInfo(t *testing.T) {
	// ok=false (e.g., older bundle without balloon) → return capacity.
	got := deriveAllocatableAtSnapshot(1<<30, 0, 0, false)
	if got != 1<<30 {
		t.Errorf("no balloon info: want=%d got=%d", 1<<30, got)
	}
}

func TestDeriveAllocatableAtSnapshot_BalloonExceedsCapacity(t *testing.T) {
	// Defensive: if balloon target somehow exceeds capacity (corrupted
	// state), don't underflow.
	got := deriveAllocatableAtSnapshot(64<<20, 128<<20, 128<<20, true)
	if got != 0 {
		t.Errorf("overrun: want=0 got=%d", got)
	}
}
