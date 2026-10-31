package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// recordingSpawnObserver records every SpawnObserver call, in order.
type recordingSpawnObserver struct {
	mu      sync.Mutex
	started []RunningSpawn
	prompts []string
	texts   []string
	ended   []string
	parents []string
}

func (o *recordingSpawnObserver) SpawnStarted(parent string, s RunningSpawn, prompt string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.started = append(o.started, s)
	o.prompts = append(o.prompts, prompt)
	o.parents = append(o.parents, parent)
}

func (o *recordingSpawnObserver) SpawnText(_, id, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.texts = append(o.texts, id+"="+text)
}

func (o *recordingSpawnObserver) SpawnEnded(_, id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ended = append(o.ended, id)
}

// blockingSpawnAgent emits an intermediate text block, then runs until its
// context is cancelled.
type blockingSpawnAgent struct{ running chan struct{} }

func (a *blockingSpawnAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	turnevent.Emit(ctx, turnevent.TextBlock{Text: "looking", Phase: turnevent.PhaseIntermediate})
	close(a.running)
	<-ctx.Done()
	turnevent.Emit(ctx, turnevent.TurnComplete{Err: ctx.Err()})
	return ctx.Err()
}

func TestSpawnInherit_ObserverListsAndStopStops(t *testing.T) {
	// A background clone spawn is reported to the observer (start with model
	// and a one-line description, its text, its end) and /stop's tracker stops
	// it on its own, the agent being told it was stopped (#2139).
	t.Parallel()
	delivered := make(chan string, 1)
	notifier := NewAsyncNotifier(func(_, msg, _, _ string, _ *AgentOrigin) { delivered <- msg })
	obs := &recordingSpawnObserver{}
	tracker := NewSpawnTracker()
	agent := &blockingSpawnAgent{running: make(chan struct{})}
	deps := SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    "test",
		MaxInherit: 2,
		Notifier:   notifier,
		Tracker:    tracker,
		Observer:   obs,
		ModelFor:   func(sk string) string { return "model-for-" + sk },
	}
	tool := NewSpawner(deps, func() SpawnAgent { return agent }).SpawnTool()
	ctx := WithSessionKey(context.Background(), "test/imain")
	params, _ := json.Marshal(map[string]string{"prompt": "Find the bug\n  in   the parser", "context": "clone"})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	select {
	case <-agent.running:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never ran")
	}

	obs.mu.Lock()
	if len(obs.started) != 1 {
		obs.mu.Unlock()
		t.Fatalf("started = %d, want 1", len(obs.started))
	}
	s := obs.started[0]
	if obs.parents[0] != "test/imain" || s.Description != "Find the bug in the parser" ||
		s.Model != "model-for-test/imain/b1000000000" || s.BranchKey != "test/imain/b1000000000" ||
		s.Started.IsZero() || strings.Contains(s.ID, ":") || obs.prompts[0] != "Find the bug\n  in   the parser" {
		t.Errorf("started = %+v under %q with prompt %q", s, obs.parents[0], obs.prompts[0])
	}
	obs.mu.Unlock()

	// The statusline lists it under its parent session only (#2127).
	if r := tracker.Running("test/imain"); len(r) != 1 || r[0] != s {
		t.Errorf("Running = %+v, want [%+v]", r, s)
	}
	if r := tracker.Running("test/imain/other"); r != nil {
		t.Errorf("Running(other session) = %+v, want none", r)
	}

	if n := tracker.Stop("test/imain/other"); n != 0 {
		t.Errorf("Stop(other session) = %d, want 0", n)
	}
	if n := tracker.Stop("test/imain"); n != 1 {
		t.Fatalf("Stop = %d, want 1", n)
	}
	select {
	case msg := <-delivered:
		if !strings.Contains(msg, "was stopped by the user") {
			t.Errorf("result = %q, want it to say the spawn was stopped", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped spawn never reported")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.ended) != 1 || obs.ended[0] != s.ID {
		t.Errorf("ended = %v, want [%s]", obs.ended, s.ID)
	}
	if len(obs.texts) != 1 || obs.texts[0] != s.ID+"=looking" {
		t.Errorf("texts = %v, want the intermediate block", obs.texts)
	}
	if n := tracker.Stop("test/imain"); n != 0 {
		t.Errorf("Stop after end = %d, want 0", n)
	}
	if r := tracker.Running("test/imain"); r != nil {
		t.Errorf("Running after end = %+v, want none", r)
	}
}

func TestSpawnTextSink_ForwardsBlocksAndAnswer(t *testing.T) {
	// The API turn sends the answer only on TurnComplete; the chit needs it too.
	obs := &recordingSpawnObserver{}
	buf := turnevent.NewBufferSink()
	sink := &spawnTextSink{BufferSink: buf, obs: obs, parent: "p", id: "s"}
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TextBlock{Text: "step"})
	sink.Emit(ctx, turnevent.ToolCall{Name: "read"})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "answer"})
	if got := strings.Join(obs.texts, "|"); got != "s=step|s=answer" {
		t.Errorf("texts = %q", got)
	}
	if buf.FinalText() != "answer" {
		t.Errorf("buffer final = %q", buf.FinalText())
	}
}

// A delegated backend (Claude Code, codex) streams every assistant message as
// a text block, then ends the turn with FinalText = all of them joined by a
// blank line. The chit already holds each block, so the join must not be added
// again as one long block (#2598: seen live on spawn-26ac4830890a8749, frame
// 245029 = frames 244951..245028 joined by "\n\n").
func TestSpawnTextSink_AccumulatedFinalTextNotRepeated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []string
		final  string
		want   []string
	}{
		{"claude code: messages joined by a blank line",
			[]string{"Checks done.", "Worktree is up.", "🏆 Landed."},
			"Checks done.\n\nWorktree is up.\n\n🏆 Landed.",
			[]string{"Checks done.", "Worktree is up.", "🏆 Landed."}},
		{"claude code: two blocks of one message, joined with no separator",
			[]string{"Hello ", "world!", "Done."},
			"Hello world!\n\nDone.",
			[]string{"Hello ", "world!", "Done."}},
		{"codex: an error appended after the streamed messages is still shown",
			[]string{"Working.", "Half done."},
			"Working.\n\nHalf done.\n\nError: turn failed",
			[]string{"Working.", "Half done.", "Error: turn failed"}},
		{"api: the answer arrives only on TurnComplete",
			[]string{"step"},
			"answer",
			[]string{"step", "answer"}},
		{"api: an answer that only ends like the last block is still shown",
			[]string{"Done."},
			"All checks passed. Done.",
			[]string{"Done.", "All checks passed. Done."}},
		{"a FinalText that is just the last block is not repeated",
			[]string{"working", "answer"},
			"answer",
			[]string{"working", "answer"}},
		{"nothing streamed: the whole answer is shown",
			nil,
			"one\n\ntwo",
			[]string{"one\n\ntwo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingSpawnObserver{}
			tr := NewSpawnTracker()
			buf := turnevent.NewBufferSink()
			sink := &spawnTextSink{BufferSink: buf, obs: obs, tracker: tr, parent: "p", id: "s"}
			tr.addDirect("p", RunningSpawn{ID: "s"}, func() {}, time.Minute, "prompt")
			ctx := context.Background()
			for _, b := range tc.blocks {
				sink.Emit(ctx, turnevent.TextBlock{Text: b, Phase: turnevent.PhaseIntermediate})
			}
			sink.Emit(ctx, turnevent.TurnComplete{FinalText: tc.final})
			var want []string
			for _, w := range tc.want {
				want = append(want, "s="+w)
			}
			if got := strings.Join(obs.texts, "|"); got != strings.Join(want, "|") {
				t.Errorf("chit texts = %q\nwant        %q", obs.texts, want)
			}
			if buf.FinalText() != tc.final {
				t.Errorf("buffer final = %q, want the full FinalText (the spawn's result)", buf.FinalText())
			}
		})
	}
}

func TestSpawnDescription(t *testing.T) {
	long := strings.Repeat("é", 150)
	for in, want := range map[string]string{
		"  a\n\tb  c ": "a b c",
		long:           strings.Repeat("é", 100) + "…",
	} {
		if got := spawnDescription(in); got != want {
			t.Errorf("spawnDescription(%q) = %q, want %q", in, got, want)
		}
	}
}
