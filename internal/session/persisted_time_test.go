package session

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPersistedTime_RoundTripAndClear(t *testing.T) {
	idx, err := NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	p := idx.PersistedTime("ag", "k")

	if _, ok := p.Load(); ok {
		t.Fatal("empty store reported a value")
	}
	want := time.Now().Add(-90 * time.Minute) // sub-second precision must survive
	if err := p.Save(want); err != nil {
		t.Fatal(err)
	}
	if got, ok := idx.PersistedTime("ag", "k").Load(); !ok || !got.Equal(want) {
		t.Errorf("Load = %v,%v; want %v", got, ok, want)
	}
	if _, ok := idx.PersistedTime("other", "k").Load(); ok {
		t.Error("value leaked to another agent")
	}
	if err := p.Save(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Load(); ok {
		t.Error("zero Save did not clear the row")
	}
	if raw, _ := idx.GetAgentMetadata("ag", "k"); raw != "" {
		t.Errorf("row still present after zero Save: %q", raw)
	}

	// Unparseable → not ok (caller keeps its boot default).
	_ = idx.SetAgentMetadata("ag", "k", "garbage")
	if _, ok := p.Load(); ok {
		t.Error("garbage parsed as a time")
	}
}

func TestPersistedTime_NilIndexIsNoOp(t *testing.T) {
	var idx *SessionIndex
	p := idx.PersistedTime("ag", "k")
	if err := p.Save(time.Now()); err != nil {
		t.Errorf("Save on nil index: %v", err)
	}
	if _, ok := p.Load(); ok {
		t.Error("Load on nil index reported a value")
	}
}
