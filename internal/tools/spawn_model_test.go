package tools

import (
	"context"
	"slices"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// TestSpawnTextSink_NotesModel proves the sink records what the backend reports
// and tells the observer once per change (#2879). A spawn run as an agent with no
// configured model starts with an empty entry, so this is its only way to get one.
func TestSpawnTextSink_NotesModel(t *testing.T) {
	t.Parallel()
	const parent, id = "cam/c1", "spawn-1"
	tr := NewSpawnTracker()
	tr.add(parent, RunningSpawn{ID: id, BranchKey: parent + "/b1", Started: time.Now(), Agent: "codex"}, func() {})
	obs := &recordingSpawnObserver{}
	sink := &spawnTextSink{BufferSink: turnevent.NewBufferSink(), obs: obs, tracker: tr, parent: parent, id: id}
	ctx := context.Background()

	for _, ev := range []turnevent.Event{
		turnevent.ModelKnown{Model: ""},
		turnevent.ModelKnown{Model: "codex/gpt-6.1-sol"},
		turnevent.ModelKnown{Model: "codex/gpt-6.1-sol"}, // no change: no second frame
		turnevent.TurnComplete{Model: "<synthetic>"},     // a sentinel, not a model
		turnevent.TurnComplete{Model: ""},                // a failed turn names none
		turnevent.TurnComplete{Model: "codex/gpt-rerouted"},
	} {
		sink.Emit(ctx, ev)
	}

	want := []string{id + "=codex/gpt-6.1-sol", id + "=codex/gpt-rerouted"}
	if !slices.Equal(obs.models, want) {
		t.Errorf("observer models = %v, want %v", obs.models, want)
	}
	if info, ok := tr.spawnInfo(parent, id); !ok || info.Model != "codex/gpt-rerouted" {
		t.Errorf("tracked model = %q (found %v), want codex/gpt-rerouted", info.Model, ok)
	}
}

// TestSetModel_UnknownSpawn: a report for a spawn the tracker does not hold (or
// no tracker) changes nothing and tells nobody.
func TestSetModel_UnknownSpawn(t *testing.T) {
	t.Parallel()
	var none *SpawnTracker
	if none.setModel("p", "x", "m") {
		t.Error("nil tracker reported a change")
	}
	if NewSpawnTracker().setModel("p", "x", "m") {
		t.Error("unknown spawn reported a change")
	}
}

// TestSetModel_Persisted proves the learned model reaches the spawn's record, so
// a spawn restored after a restart still names it.
func TestSetModel_Persisted(t *testing.T) {
	t.Parallel()
	store := newMemSpawnStore()
	tr := NewSpawnTracker()
	tr.SetStore(store)
	tr.addWithPrompt("test/c1", RunningSpawn{ID: "spawn-1", BranchKey: "test/c1/b1", Started: time.Now()}, func() {}, "p")
	if !tr.setModel("test/c1", "spawn-1", "codex/gpt-6.1-sol") {
		t.Fatal("setModel reported no change")
	}
	rec, ok := store.record(t, "spawn-1")
	if !ok {
		t.Fatal("no persisted record")
	}
	if rec.Model != "codex/gpt-6.1-sol" {
		t.Errorf("persisted model = %q", rec.Model)
	}
}
