package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/turnevent"
)

// #2683 (Dick 2026-10-30): a direct spawn shows in the app as a subagent, the
// same as a background spawn: started with its prompt, its text, ended.

func newObservedDirectSpawner(agentID string) (*Spawner, *recordingSpawnObserver, *SpawnTracker) {
	obs := &recordingSpawnObserver{}
	tr := NewSpawnTracker()
	s := NewSpawner(SpawnDeps{AgentID: agentID, MaxInherit: 1, Tracker: tr, Observer: obs},
		func() SpawnAgent { return &busyAgent{} })
	return s, obs, tr
}

// A delegated batch's events reach the observer through the batch turn's tee
// while the call waits; the answer, already shown, is not shown again.
func TestDirectSpawn_ObserverShowsTeedText(t *testing.T) {
	t.Parallel()
	s, obs, tr := newObservedDirectSpawner("dobs1")
	ctx := WithSessionKey(context.Background(), "dobs1/c1")
	res, err := s.runDirect(ctx, s, spawnModeExplore, "Look\n  around", "", time.Minute, func(runCtx context.Context) (ToolResult, error) {
		obs.mu.Lock()
		started := append([]RunningSpawn(nil), obs.started...)
		obs.mu.Unlock()
		if len(started) != 1 || !started[0].Direct || started[0].Mode != spawnModeExplore ||
			started[0].Description != "Look around" || started[0].Started.IsZero() || strings.Contains(started[0].ID, ":") {
			t.Errorf("while running: started = %+v, want the direct spawn", started)
		}
		if r := tr.Running("dobs1/c1"); len(r) != 1 {
			t.Errorf("while running: Running = %+v, want it", r)
		}
		tee := turnevent.TeeFromContext(runCtx)
		if tee == nil {
			t.Fatal("the run's ctx has no tee, so a batch's text cannot reach the app")
		}
		tee.Emit(runCtx, turnevent.TextBlock{Text: "working", Phase: turnevent.PhaseIntermediate})
		tee.Emit(runCtx, turnevent.TextBlock{Text: "answer"})
		tee.Emit(runCtx, turnevent.TurnComplete{FinalText: "answer"})
		return TextResult("answer"), nil
	})
	if err != nil || res.Text != "answer" {
		t.Fatalf("result = %q, %v", res.Text, err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	id := obs.started[0].ID
	if obs.parents[0] != "dobs1/c1" || obs.prompts[0] != "Look\n  around" {
		t.Errorf("started under %q with prompt %q", obs.parents[0], obs.prompts[0])
	}
	if want := []string{id + "=working", id + "=answer", id + "=answer"}; strings.Join(obs.texts, "|") != strings.Join(want, "|") {
		// TextBlock + TurnComplete each forward, as spawnTextSink does for a
		// background spawn; runDirect adds nothing once text has streamed.
		t.Errorf("texts = %v, want %v", obs.texts, want)
	}
	if len(obs.ended) != 1 || obs.ended[0] != id {
		t.Errorf("ended = %v, want [%s]", obs.ended, id)
	}
	if r := tr.Running("dobs1/c1"); r != nil {
		t.Errorf("after the end: Running = %+v, want none", r)
	}
}

// An API one-shot streams no text: its answer is shown when it ends.
func TestDirectSpawn_ObserverShowsAnswerWhenNothingStreamed(t *testing.T) {
	t.Parallel()
	s, obs, _ := newObservedDirectSpawner("dobs2")
	ctx := WithSessionKey(context.Background(), "dobs2/c1")
	if _, err := s.runDirect(ctx, s, spawnModeRaw, "p", "", time.Minute, func(context.Context) (ToolResult, error) {
		return TextResult("plain answer"), nil
	}); err != nil {
		t.Fatal(err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.started) != 1 || len(obs.ended) != 1 {
		t.Fatalf("started %d, ended %d; want 1 each", len(obs.started), len(obs.ended))
	}
	id := obs.started[0].ID
	if len(obs.texts) != 1 || obs.texts[0] != id+"=plain answer" {
		t.Errorf("texts = %v, want the answer", obs.texts)
	}
}

// "/stop subagents" (and the turn's /stop) runs SpawnTracker.Stop on the
// parent: it stops the direct spawn, which the observer then ends.
func TestDirectSpawn_StopSubagentsStopsIt(t *testing.T) {
	t.Parallel()
	s, obs, tr := newObservedDirectSpawner("dobs3")
	ctx := WithSessionKey(context.Background(), "dobs3/c1")
	running := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "p", "", time.Minute, func(runCtx context.Context) (ToolResult, error) {
			close(running)
			<-runCtx.Done()
			return ToolResult{}, runCtx.Err()
		})
		done <- err
	}()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn never ran")
	}
	if n := tr.Stop("dobs3/c1"); n != 1 {
		t.Fatalf("Stop = %d, want 1", n)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped by the user") {
			t.Errorf("err = %v, want stopped by the user", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped spawn never returned")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.ended) != 1 || obs.ended[0] != obs.started[0].ID {
		t.Errorf("ended = %v, want the spawn", obs.ended)
	}
}

// teeCheckingRunner records whether each batch's ctx carries a tee.
type teeCheckingRunner struct{ teed []bool }

func (r *teeCheckingRunner) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	r.teed = append(r.teed, turnevent.TeeFromContext(ctx) != nil)
	return "answer for " + req.Purpose, nil
}

// Through the tool, every direct mode (raw, explore, character) reaches the
// batch runner with the tee, and is shown.
func TestDirectSpawn_EveryModeTeesItsBatch(t *testing.T) {
	t.Parallel()
	runner := &teeCheckingRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	obs := &recordingSpawnObserver{}
	s.deps.Tracker = NewSpawnTracker()
	s.deps.Observer = obs
	ctx := WithSessionKey(context.Background(), "test/c1")
	for _, mode := range []string{spawnModeRaw, spawnModeExplore, spawnModeCharacter} {
		params, _ := json.Marshal(map[string]string{"prompt": "p", "context": mode})
		if _, err := s.SpawnTool().Execute(ctx, params); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if len(runner.teed) != 3 || !runner.teed[0] || !runner.teed[1] || !runner.teed[2] {
		t.Errorf("teed = %v, want every batch teed", runner.teed)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.started) != 3 || len(obs.ended) != 3 {
		t.Errorf("started %d, ended %d; want 3 each", len(obs.started), len(obs.ended))
	}
}

// #2685 (Dick 2026-10-30): a direct spawn's entry names the model it runs on.
func TestDirectSpawn_EntryNamesItsModel(t *testing.T) {
	t.Parallel()
	t.Run("delegated", func(t *testing.T) {
		t.Parallel()
		s := newDelegatedSpawner(&teeCheckingRunner{}, allBatchCaps)
		obs := &recordingSpawnObserver{}
		s.deps.Tracker = NewSpawnTracker()
		s.deps.Observer = obs
		s.deps.ModelFor = func(sk string) string { return "model-of:" + sk }
		ctx := WithSessionKey(context.Background(), "test/c1/b9")
		for _, model := range []string{"", "sonnet"} {
			params, _ := json.Marshal(map[string]string{"prompt": "p", "context": spawnModeExplore, "model": model})
			if _, err := s.SpawnTool().Execute(ctx, params); err != nil {
				t.Fatal(err)
			}
		}
		obs.mu.Lock()
		defer obs.mu.Unlock()
		// No model: the batch runs on its session's, which reads the owner's root.
		if len(obs.started) != 2 || obs.started[0].Model != "model-of:test/c1" || obs.started[1].Model != "sonnet" {
			t.Errorf("started = %+v, want models model-of:test/c1 then sonnet", obs.started)
		}
	})
	t.Run("api", func(t *testing.T) {
		t.Parallel()
		obs := &recordingSpawnObserver{}
		s := NewSpawner(SpawnDeps{AgentID: "dobs6", MaxInherit: 1, Tracker: NewSpawnTracker(), Observer: obs, FallbackModel: "anthropic/claude-x"},
			func() SpawnAgent { return &busyAgent{} })
		ctx := WithSessionKey(context.Background(), "dobs6/c1")
		if _, err := s.runDirect(ctx, s, spawnModeRaw, "p", "", time.Minute, func(context.Context) (ToolResult, error) {
			return TextResult("ok"), nil
		}); err != nil {
			t.Fatal(err)
		}
		obs.mu.Lock()
		defer obs.mu.Unlock()
		if len(obs.started) != 1 || obs.started[0].Model != "anthropic/claude-x" {
			t.Errorf("started = %+v, want the resolved model", obs.started)
		}
	})
}
