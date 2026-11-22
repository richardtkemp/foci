package app

import (
	"slices"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/fap"
	"foci/internal/tools"
)

// TestSpawnObserver_HeldSpawnIsNotActivity proves a spawn the usage hold
// paused is held work, not activity (#3256): the frame goes idle with no
// running list, and carries the lift time and the spawn's id, exactly as a
// spawn waiting for the usage reset does (#3146). The chat stays exempt from
// auto-close. Before, a paused spawn kept the chat "subagents"-active: the
// roster showed activity dots for a chat whose only work was paused.
func TestSpawnObserver_HeldSpawnIsNotActivity(t *testing.T) {
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

	const lift = 1_800_000_000_000
	obs.SpawnHeld(b.key(), "spawn-1", true, lift)
	f := last()
	if f["kind"] != "idle" || f["subagents"] != nil || f["usageResetMs"] != float64(lift) ||
		!slices.Equal(anyStrings(f["usageWaitIds"]), []string{"spawn-1"}) {
		t.Fatalf("held frame = %v, want idle, no subagents, usageResetMs %d, usageWaitIds [spawn-1]", f, int64(lift))
	}
	if info := b.info(); info.Activity != "idle" || len(info.Subagents) != 0 || info.UsageResetMs != lift ||
		!slices.Equal(info.UsageWaitIDs, []string{"spawn-1"}) {
		t.Fatalf("roster snapshot = %+v, want idle with the lift time and the held spawn", info)
	}
	if b.activityIdle() {
		t.Error("activityIdle = true while a spawn is paused; the chat must stay open")
	}

	// A hold with no known lift time: still held, still no activity, still
	// exempt from auto-close.
	obs.SpawnHeld(b.key(), "spawn-1", true, 0)
	f = last()
	if f["kind"] != "idle" || f["subagents"] != nil || f["usageResetMs"] != nil ||
		!slices.Equal(anyStrings(f["usageWaitIds"]), []string{"spawn-1"}) {
		t.Fatalf("held frame, no lift time = %v, want idle with usageWaitIds [spawn-1]", f)
	}
	if b.activityIdle() {
		t.Error("activityIdle = true while a spawn is paused with no lift time")
	}

	// Real activity beside the held spawn (a background command) shows as
	// activity (requirement 4); the held spawn stays out of the list.
	b.setSubagents("sleep", []fap.RunningSubagent{{ID: "cmd-1", Description: "sleep", Kind: delegator.SubagentKindCommand}})
	f = last()
	list, _ := f["subagents"].([]any)
	if f["kind"] != "subagents" || len(list) != 1 || list[0].(map[string]any)["id"] != "cmd-1" {
		t.Fatalf("frame with a command = %v, want subagents with only the command", f)
	}
	b.setSubagents("", nil)
	drain(t, client)

	// At lift the spawn is running again.
	obs.SpawnHeld(b.key(), "spawn-1", false, 0)
	f = last()
	list, _ = f["subagents"].([]any)
	if f["kind"] != "subagents" || len(list) != 1 || f["usageWaitIds"] != nil {
		t.Fatalf("frame after the lift = %v, want the spawn running again", f)
	}

	// A held spawn that ends leaves no hold behind.
	obs.SpawnHeld(b.key(), "spawn-1", true, lift)
	drain(t, client)
	obs.SpawnEnded(b.key(), "spawn-1")
	b.mu.Lock()
	n := len(b.spawnHolds)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("spawnHolds after the spawn ended = %d entries, want none", n)
	}
	if !b.activityIdle() {
		t.Error("activityIdle = false with nothing left in flight")
	}
}

// fakeHeldInputs is a waitInputs agent whose session input the usage hold
// holds (#3256).
type fakeHeldInputs struct {
	fakeWaitInputs
	held bool
	lift int64
}

func (f *fakeHeldInputs) UsageHeldInputLift(string) (bool, time.Time) {
	if !f.held {
		return false, time.Time{}
	}
	return true, time.UnixMilli(f.lift)
}

// TestHub_HeldInputIsHeldWork proves a chat whose input the usage hold holds
// shows as held (#3256): no activity, the lift time in the activity frame,
// and it stays exempt from auto-close. When the input is released the hold
// is gone.
func TestHub_HeldInputIsHeldWork(t *testing.T) {
	h := newTestHub()
	in := &fakeHeldInputs{held: true, lift: 1_800_000_000_000}
	h.waitInputsFor = func(string) waitInputs { return in }
	b := h.ensureBinding(nil, "cam", "conv-1")
	b.setIdent("cam", "cam/c1")
	client := fakeClient()
	b.attach(client)
	drain(t, client)

	h.refreshInputHeld(b)
	frames := activityFrames(drain(t, client))
	if len(frames) != 1 || frames[0]["kind"] != "idle" || frames[0]["usageResetMs"] != float64(in.lift) {
		t.Fatalf("frames with held input = %v, want one idle frame with usageResetMs %d", frames, in.lift)
	}
	if b.activityIdle() {
		t.Error("activityIdle = true while the hold holds the chat's input")
	}

	in.held = false
	h.refreshInputHeld(b)
	frames = activityFrames(drain(t, client))
	if len(frames) != 1 || frames[0]["usageResetMs"] != nil {
		t.Fatalf("frames after release = %v, want one frame with no usage wait", frames)
	}
	if !b.activityIdle() {
		t.Error("activityIdle = false after the held input was released")
	}
}
