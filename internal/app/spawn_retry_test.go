package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"foci/internal/tools"
	"foci/internal/turnevent"
)

// TestSpawnObserver_UsageWaitIsNotActivity proves a spawn waiting for the
// usage reset (a 429 retry) leaves the activity frame idle and the running
// list empty, and carries only the reset time and its id (#3146; Dick,
// 2026-11-16: "show no activity when waiting for a usage reset"). Any other
// retry wait stays activity, on the spawn's entry. Before, the waiting spawn
// kept the chat "subagents"-active: roster dots and a "thinking" spawn.
func TestSpawnObserver_UsageWaitIsNotActivity(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	b := h.ensureBinding(nil, "cam", "conv-1")
	client := fakeClient()
	b.attach(client)

	obs := SpawnObserver{}
	obs.SpawnStarted(b.key(), tools.RunningSpawn{ID: "spawn-1", Name: "reviewer"}, "review")
	drain(t, client)

	last := func() map[string]any {
		t.Helper()
		frames := activityFrames(drain(t, client))
		if len(frames) != 1 {
			t.Fatalf("activity frames = %v, want exactly one", frames)
		}
		return frames[0]
	}

	const reset = 1_800_000_000_000
	obs.SpawnRetry(b.key(), "spawn-1", reset, 429)
	f := last()
	if f["kind"] != "idle" || f["subagents"] != nil || f["usageResetMs"] != float64(reset) ||
		!slices.Equal(anyStrings(f["usageWaitIds"]), []string{"spawn-1"}) {
		t.Fatalf("usage wait frame = %v, want idle, no subagents, usageResetMs %d, usageWaitIds [spawn-1]", f, int64(reset))
	}
	if info := b.info(); info.Activity != "idle" || len(info.Subagents) != 0 || info.UsageResetMs != reset {
		t.Fatalf("roster snapshot = %+v, want idle with the reset time", info)
	}
	if b.activityIdle() {
		t.Error("activityIdle = true during a usage wait; the chat must stay open while its spawn waits")
	}

	obs.SpawnRetry(b.key(), "spawn-1", reset, 529)
	f = last()
	list, _ := f["subagents"].([]any)
	if f["kind"] != "subagents" || len(list) != 1 || list[0].(map[string]any)["retryUntilMs"] != float64(reset) || f["usageResetMs"] != nil {
		t.Fatalf("overload retry frame = %v, want the spawn listed with its retry time, no usage wait", f)
	}

	obs.SpawnRetry(b.key(), "spawn-1", 0, 0)
	f = last()
	list, _ = f["subagents"].([]any)
	if f["kind"] != "subagents" || len(list) != 1 || list[0].(map[string]any)["retryUntilMs"] != nil || f["usageResetMs"] != nil {
		t.Fatalf("cleared frame = %v, want the spawn listed with no wait", f)
	}
}

// TestAppSink_UsageWaitIsNotActivity proves the agent's own 429 retry is not
// activity either (#3146): the frame goes idle with the reset time, and the
// turn's next output brings its activity back and drops the wait.
func TestAppSink_UsageWaitIsNotActivity(t *testing.T) {
	ctx := context.Background()
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.ToolCall{Name: "Read", ID: "t1", Args: []byte(`{"file_path":"/x"}`)})
	drain(t, c)

	const delay = 90 * time.Minute
	before := time.Now()
	s.Emit(ctx, turnevent.BackendRetry{Endpoint: "Claude Code", Attempt: 1, Delay: delay, Status: 429, Category: "rate_limit"})
	after := time.Now()
	frames := activityFrames(drain(t, c))
	if len(frames) != 1 || frames[0]["kind"] != "idle" || frames[0]["agentKind"] != nil {
		t.Fatalf("frames after a 429 retry = %v, want one idle frame with no agent activity", frames)
	}
	until, _ := frames[0]["usageResetMs"].(float64)
	if lo, hi := before.Add(delay).Round(time.Second).UnixMilli(), after.Add(delay).Round(time.Second).UnixMilli(); int64(until) < lo || int64(until) > hi {
		t.Errorf("usageResetMs = %v, want in [%d, %d]", until, lo, hi)
	}
	if info := b.info(); info.Activity != "idle" || info.UsageResetMs != int64(until) {
		t.Errorf("roster snapshot = %+v, want idle with the reset time", info)
	}

	s.Emit(ctx, turnevent.TextDelta{Delta: "back"})
	frames = activityFrames(drain(t, c))
	if len(frames) != 1 || frames[0]["kind"] != "typing" || frames[0]["usageResetMs"] != nil {
		t.Fatalf("frames after output = %v, want one typing frame with no usage wait", frames)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
