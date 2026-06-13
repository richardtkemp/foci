package turn

import (
	"path/filepath"
	"testing"

	"foci/internal/tooldetail"
)

// TestToolResultStore_StoreLoadExpanded proves StoreEntry records an entry
// retrievable by Load with all fields intact, IsExpanded reflects the stored
// flag, and both report the zero value / false for unknown message IDs.
func TestToolResultStore_StoreLoadExpanded(t *testing.T) {
	var s ToolResultStore // zero value is ready to use

	if _, ok := s.Load("missing"); ok {
		t.Error("Load(missing) should report not-ok")
	}
	if s.IsExpanded("missing") {
		t.Error("IsExpanded(missing) should be false")
	}

	s.StoreEntry("100", "compact", "full", "result", true)

	got, ok := s.Load("100")
	if !ok {
		t.Fatal("entry not stored")
	}
	if got.CompactText != "compact" || got.FullInput != "full" || got.Result != "result" || !got.Expanded {
		t.Errorf("unexpected entry: %+v", got)
	}
	if !s.IsExpanded("100") {
		t.Error("IsExpanded(100) = false, want true")
	}
}

// TestToolResultStore_Update proves Update overwrites the cached entry, which is
// how the button-callback handlers record a new expand/collapse state.
func TestToolResultStore_Update(t *testing.T) {
	var s ToolResultStore
	s.StoreEntry("7", "c", "f", "", false)

	s.Update("7", ToolResultEntry{CompactText: "c", FullInput: "f", Result: "r", Expanded: true})

	got, _ := s.Load("7")
	if !got.Expanded || got.Result != "r" {
		t.Errorf("Update not applied: %+v", got)
	}
}

// TestToolResultStore_Persist proves Persist is a no-op without a detail store
// and a non-numeric ID, and write-throughs to the SQLite store (readable via
// LoadAll) when one is configured.
func TestToolResultStore_Persist(t *testing.T) {
	var s ToolResultStore

	s.Persist("100", "c", "f", "r") // no detail store: must not panic

	store, err := tooldetail.NewStore(filepath.Join(t.TempDir(), "details.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer func() { _ = store.Close() }()
	s.SetDetailStore(store, nil)

	s.Persist("abc", "c", "f", "r") // non-numeric ID: skipped, no panic
	s.Persist("100", "c", "f", "r")

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := entries[100]; !ok {
		t.Fatal("persisted entry not found")
	}
	if _, ok := entries[0]; ok {
		t.Error("non-numeric ID should not have persisted")
	}
}

// TestToolResultStore_SetDetailStore proves SetDetailStore warms the in-memory
// cache from entries already on disk (keyed by their string message ID), and
// that a nil store is a safe no-op that restores nothing.
func TestToolResultStore_SetDetailStore(t *testing.T) {
	store, err := tooldetail.NewStore(filepath.Join(t.TempDir(), "details.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer func() { _ = store.Close() }()
	store.Store(99, "compact", "full input", "result text")

	var s ToolResultStore
	s.SetDetailStore(store, nil)

	got, ok := s.Load("99")
	if !ok {
		t.Fatal("entry not restored from disk")
	}
	if got.CompactText != "compact" || got.FullInput != "full input" || got.Result != "result text" {
		t.Errorf("unexpected restored entry: %+v", got)
	}

	var empty ToolResultStore
	empty.SetDetailStore(nil, nil) // nil store: no panic, restores nothing
	if _, ok := empty.Load("99"); ok {
		t.Error("nil store should restore nothing")
	}
}
