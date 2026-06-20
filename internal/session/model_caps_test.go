package session

import (
	"testing"
	"time"
)

func TestModelCaps_SaveLoadRoundTrip(t *testing.T) {
	// Proves SaveModelCaps then LoadModelCaps round-trips rows verbatim and
	// preserves the fetched_at stamp (truncated to RFC3339 second precision).
	idx := tempIndex(t)
	fetchedAt := time.Now().UTC().Truncate(time.Second)

	rows := []ModelCapsRow{
		{Model: "claude-opus-4-8", ContextWindow: 1000000, MaxOutput: 64000, EffortJSON: `["low","high","max"]`, ThinkingJSON: `["adaptive"]`},
		{Model: "claude-sonnet-4-6", ContextWindow: 200000, MaxOutput: 64000, EffortJSON: "", ThinkingJSON: ""},
	}
	if err := idx.SaveModelCaps("ccstream", rows, fetchedAt); err != nil {
		t.Fatalf("SaveModelCaps: %v", err)
	}

	got, gotAt, err := idx.LoadModelCaps("ccstream")
	if err != nil {
		t.Fatalf("LoadModelCaps: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if !gotAt.Equal(fetchedAt) {
		t.Errorf("fetchedAt = %v, want %v", gotAt, fetchedAt)
	}
	byModel := map[string]ModelCapsRow{}
	for _, r := range got {
		byModel[r.Model] = r
	}
	opus := byModel["claude-opus-4-8"]
	if opus.ContextWindow != 1000000 || opus.MaxOutput != 64000 || opus.EffortJSON != `["low","high","max"]` || opus.ThinkingJSON != `["adaptive"]` {
		t.Errorf("opus round-trip wrong: %+v", opus)
	}
	if s := byModel["claude-sonnet-4-6"]; s.EffortJSON != "" || s.ThinkingJSON != "" {
		t.Errorf("sonnet empty levels not preserved: %+v", s)
	}
}

func TestModelCaps_SaveReplacesBackend(t *testing.T) {
	// Proves a second Save fully replaces a backend's rows (no stale leftovers)
	// and that backends are isolated from each other.
	idx := tempIndex(t)
	at := time.Now().UTC().Truncate(time.Second)

	if err := idx.SaveModelCaps("api", []ModelCapsRow{
		{Model: "old-model", ContextWindow: 100},
		{Model: "claude-opus-4-8", ContextWindow: 200},
	}, at); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// A separate backend must not be touched by the api replace below.
	if err := idx.SaveModelCaps("ccstream", []ModelCapsRow{{Model: "cc-only", ContextWindow: 1}}, at); err != nil {
		t.Fatalf("ccstream Save: %v", err)
	}
	// Replace api with a smaller set — old-model must vanish.
	if err := idx.SaveModelCaps("api", []ModelCapsRow{{Model: "claude-opus-4-8", ContextWindow: 999}}, at); err != nil {
		t.Fatalf("replace Save: %v", err)
	}

	api, _, err := idx.LoadModelCaps("api")
	if err != nil {
		t.Fatalf("LoadModelCaps api: %v", err)
	}
	if len(api) != 1 || api[0].Model != "claude-opus-4-8" || api[0].ContextWindow != 999 {
		t.Errorf("api not replaced cleanly: %+v", api)
	}
	cc, _, _ := idx.LoadModelCaps("ccstream")
	if len(cc) != 1 || cc[0].Model != "cc-only" {
		t.Errorf("ccstream backend leaked/lost: %+v", cc)
	}
}

func TestModelCaps_LoadEmptyBackend(t *testing.T) {
	// Proves an unknown backend returns (nil, zero, nil) — a cold cache, not an
	// error — so the caller falls back to the static registry.
	idx := tempIndex(t)
	rows, at, err := idx.LoadModelCaps("never-saved")
	if err != nil {
		t.Fatalf("LoadModelCaps: %v", err)
	}
	if rows != nil || !at.IsZero() {
		t.Errorf("empty backend should be cold: rows=%v at=%v", rows, at)
	}
}
