package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// #3166 (work Mac, 18 Nov): a direct spawn's call was cancelled (its calling
// shell killed at its time limit), and its run did not end, because the codex
// backend's close left its turn open. The spawn stayed 'running' in
// spawn_status for good, while spawn_stop said it was not running. Its end must
// not depend on the run honouring its cancellation.

// stuckRun is a direct spawn's run that ignores its ctx until released.
func stuckRun(release <-chan struct{}) func(context.Context) (ToolResult, error) {
	return func(context.Context) (ToolResult, error) {
		<-release
		return TextResult("too late"), nil
	}
}

func directSnap(t *testing.T, s *Spawner, parent string) SpawnSnapshot {
	t.Helper()
	l := s.deps.Tracker.List(parent)
	if len(l) != 1 {
		t.Fatalf("tracker lists %d spawns for %s, want 1", len(l), parent)
	}
	return l[0]
}

func waitRunning(t *testing.T, s *Spawner, parent string) SpawnSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := s.deps.Tracker.List(parent); len(l) == 1 && l[0].Running {
			return l[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the direct spawn never showed as running")
	return SpawnSnapshot{}
}

// The caller's call is cancelled and the run ignores it: within the grace the
// spawn ends (failed), and spawn_status and spawn_stop agree it is not running.
func TestDirectSpawnCallCancelledRunStuckEnds(t *testing.T) {
	t.Parallel()
	s := NewSpawner(SpawnDeps{AgentID: "dcancel", MaxInherit: 1, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{} })
	s.directEndGrace = 50 * time.Millisecond
	const parent = "dcancel/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	release := make(chan struct{})
	defer close(release)

	type out struct {
		res ToolResult
		err error
	}
	got := make(chan out, 1)
	go func() {
		r, err := s.runDirect(ctx, s, spawnModeCharacter, "stuck", "p", "", stuckRun(release))
		got <- out{r, err}
	}()
	snap := waitRunning(t, s, parent)
	cancel() // the calling shell was killed

	select {
	case o := <-got:
		if o.err == nil {
			t.Errorf("runDirect = %q, nil; want an error: its call was cancelled", o.res.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDirect did not return after its call was cancelled and the grace passed")
	}
	after := directSnap(t, s, parent)
	if after.Running || after.Outcome != "failed" {
		t.Errorf("after cancel: running=%v outcome=%q, want ended, failed", after.Running, after.Outcome)
	}
	if r, err := s.stop(parent, snap.ID); err != nil || !strings.Contains(r.Text, "is not running (failed ") {
		t.Errorf("spawn_stop = %q, %v; want it not running, as spawn_status says", r.Text, err)
	}
}

// spawn_stop on a direct spawn whose run ignores it: the spawn ends, stopped,
// within the grace; a second spawn_stop meanwhile says it is already stopping,
// not that it is not running while spawn_status says it runs.
func TestDirectSpawnStoppedRunStuckEnds(t *testing.T) {
	t.Parallel()
	s := NewSpawner(SpawnDeps{AgentID: "dstop", MaxInherit: 1, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{} })
	s.directEndGrace = 300 * time.Millisecond
	const parent = "dstop/c1"
	ctx := WithSessionKey(context.Background(), parent)
	release := make(chan struct{})
	defer close(release)

	got := make(chan error, 1)
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "stuck", "p", "", stuckRun(release))
		got <- err
	}()
	snap := waitRunning(t, s, parent)
	if r, err := s.stop(parent, snap.ID); err != nil || !strings.Contains(r.Text, "its call returns as stopped") {
		t.Fatalf("spawn_stop = %q, %v; want stopping", r.Text, err)
	}
	if r, err := s.stop(parent, snap.ID); err != nil || !strings.Contains(r.Text, "already being stopped") {
		t.Errorf("second spawn_stop = %q, %v; want 'already being stopped' while its run has not ended", r.Text, err)
	}
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "was stopped by") {
			t.Errorf("runDirect err = %v, want stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDirect did not return after spawn_stop and the grace passed")
	}
	if after := directSnap(t, s, parent); after.Running || after.Outcome != "stopped" {
		t.Errorf("after stop: running=%v outcome=%q, want ended, stopped", after.Running, after.Outcome)
	}
}

// A run that honours its cancellation ends at once, not after the grace.
func TestDirectSpawnCallCancelledRunEndsPromptly(t *testing.T) {
	t.Parallel()
	s := NewSpawner(SpawnDeps{AgentID: "dprompt", MaxInherit: 1, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{} })
	s.directEndGrace = time.Hour
	const parent = "dprompt/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	got := make(chan error, 1)
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "quick", "p", "", func(runCtx context.Context) (ToolResult, error) {
			<-runCtx.Done()
			return ToolResult{}, runCtx.Err()
		})
		got <- err
	}()
	waitRunning(t, s, parent)
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Error("runDirect err = nil, want the cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDirect waited out the grace for a run that ended")
	}
	if after := directSnap(t, s, parent); after.Running {
		t.Error("spawn still running after its run ended")
	}
}
