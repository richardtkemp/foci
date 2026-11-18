package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// #3168 (Dick 2026-11-18 19:41): a direct spawn whose call died without a
// stop delivers its kept send_to_spawner messages and its result (or the text
// of its interrupted turn) to its spawner as a late report; a stopped one
// delivers nothing; nothing is delivered twice.

func newLateSpawner(t *testing.T, agent string) (*Spawner, chan injected) {
	t.Helper()
	notifier, got := recordingNotifier()
	// The caller's turn runs: the spawn's send_to_spawner messages are kept
	// for the head of its result.
	s := NewSpawner(SpawnDeps{AgentID: agent, MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{busy: true} })
	return s, got
}

// keepNote has the running spawn keep a message for its spawner.
func keepNote(t *testing.T, s *Spawner, runCtx context.Context, text string) {
	t.Helper()
	if r, err := sendToSpawner(s, runCtx, text); err != nil || !strings.HasPrefix(r.Text, "Kept:") {
		t.Errorf("send_to_spawner = %q, %v; want kept", r.Text, err)
	}
}

// writeText streams an assistant text block through the spawn's tee.
func writeText(runCtx context.Context, text string) {
	turnevent.TeeFromContext(runCtx).Emit(runCtx, turnevent.TextBlock{Text: text})
}

func noInject(t *testing.T, got <-chan injected, wait time.Duration) {
	t.Helper()
	select {
	case m := <-got:
		t.Fatalf("unexpected delivery to %s: %q", m.session, m.msg)
	case <-time.After(wait):
	}
}

// The call dies; the run honours it and ends interrupted: its kept message
// and the text it had written reach the spawner once.
func TestDirectSpawnCallDiedDeliversKeptAndPartial(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late1")
	const parent = "late1/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	ret := make(chan error, 1)
	wrote := make(chan struct{})
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "lp", "p", "", func(runCtx context.Context) (ToolResult, error) {
			keepNote(t, s, runCtx, "held note")
			writeText(runCtx, "half an answer")
			close(wrote)
			<-runCtx.Done()
			return ToolResult{}, runCtx.Err()
		})
		ret <- err
	}()
	snap := waitRunning(t, s, parent)
	<-wrote
	cancel()
	if err := recv(t, ret, "runDirect's return"); err == nil {
		t.Error("runDirect err = nil, want its call's cancellation")
	}
	m := recv(t, got, "the late report")
	if m.session != parent {
		t.Errorf("late report went to %q, want %q", m.session, parent)
	}
	for _, want := range []string{"[MESSAGES FROM YOUR SPAWN] Spawn " + snap.ID, "held note",
		"[SPAWN RESULT, AFTER ITS CALL ENDED]", "It was cut off", "half an answer"} {
		if !strings.Contains(m.msg, want) {
			t.Errorf("late report lacks %q:\n%s", want, m.msg)
		}
	}
	noInject(t, got, 100*time.Millisecond)
}

// The call dies, and the run completes after it (within the grace): its full
// result reaches the spawner.
func TestDirectSpawnCallDiedDeliversLaterResult(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late2")
	s.directEndGrace = 5 * time.Second
	const parent = "late2/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	release := make(chan struct{})
	go func() {
		_, _ = s.runDirect(ctx, s, spawnModeCharacter, "lr", "p", "", func(runCtx context.Context) (ToolResult, error) {
			<-release
			return TextResult("the full answer"), nil
		})
	}()
	waitRunning(t, s, parent)
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	m := recv(t, got, "the late report")
	if !strings.Contains(m.msg, "It completed:\n\nthe full answer") || strings.Contains(m.msg, "[MESSAGES FROM YOUR SPAWN]") {
		t.Errorf("late report = %q, want the completed result and no messages head", m.msg)
	}
	noInject(t, got, 100*time.Millisecond)
}

// spawn_stop: the spawn's kept message and text go nowhere late, even though
// its caller's call also dies afterwards.
func TestDirectSpawnStoppedDeliversNothingLate(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late3")
	const parent = "late3/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	defer cancel()
	ret := make(chan error, 1)
	wrote := make(chan struct{})
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "st", "p", "", func(runCtx context.Context) (ToolResult, error) {
			keepNote(t, s, runCtx, "held note")
			writeText(runCtx, "half an answer")
			close(wrote)
			<-runCtx.Done()
			cancel() // the caller goes too
			return ToolResult{}, runCtx.Err()
		})
		ret <- err
	}()
	snap := waitRunning(t, s, parent)
	<-wrote
	if r, err := s.stop(parent, snap.ID); err != nil || !strings.Contains(r.Text, "its call returns as stopped") {
		t.Fatalf("spawn_stop = %q, %v", r.Text, err)
	}
	if err := recv(t, ret, "runDirect's return"); err == nil || !strings.Contains(err.Error(), "was stopped by") {
		t.Errorf("runDirect err = %v, want stopped", err)
	}
	noInject(t, got, 200*time.Millisecond)
}

// The run outlives its grace: its kept message goes at once, its result when
// the run ends, each once.
func TestDirectSpawnCallDiedRunStuckDeliversKeptThenResult(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late4")
	s.directEndGrace = 50 * time.Millisecond
	const parent = "late4/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	release := make(chan struct{})
	kept := make(chan struct{})
	go func() {
		_, _ = s.runDirect(ctx, s, spawnModeCharacter, "stk", "p", "", func(runCtx context.Context) (ToolResult, error) {
			keepNote(t, s, runCtx, "held note")
			close(kept)
			<-release
			return TextResult("finally done"), nil
		})
	}()
	waitRunning(t, s, parent)
	<-kept
	cancel()
	first := recv(t, got, "the kept messages report")
	if !strings.Contains(first.msg, "held note") || !strings.Contains(first.msg, "has not ended yet") || strings.Contains(first.msg, "finally done") {
		t.Errorf("first report = %q, want the kept message and a note that the result follows", first.msg)
	}
	close(release)
	second := recv(t, got, "the result report")
	if !strings.Contains(second.msg, "It completed:\n\nfinally done") || strings.Contains(second.msg, "held note") {
		t.Errorf("second report = %q, want the result only", second.msg)
	}
	noInject(t, got, 100*time.Millisecond)
}

// A spawn whose call died before it wrote or kept anything sends no report.
func TestDirectSpawnCallDiedNothingProducedNoReport(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late5")
	const parent = "late5/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	ret := make(chan error, 1)
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeCharacter, "nil", "p", "", func(runCtx context.Context) (ToolResult, error) {
			<-runCtx.Done()
			return ToolResult{}, runCtx.Err()
		})
		ret <- err
	}()
	waitRunning(t, s, parent)
	cancel()
	recv(t, ret, "runDirect's return")
	noInject(t, got, 100*time.Millisecond)
}

// An API one-shot cut off by a shutdown is resumed after the restart, and the
// resumed run delivers its kept messages and result (#2979): no late report
// now, or they would arrive twice.
func TestDirectSpawnCallDiedResumedOneShotNoReport(t *testing.T) {
	t.Parallel()
	s, got := newLateSpawner(t, "late6")
	const parent = "late6/c1"
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), parent))
	ret := make(chan error, 1)
	wrote := make(chan struct{})
	go func() {
		_, err := s.runDirect(ctx, s, spawnModeRaw, "os", "p", "", func(runCtx context.Context) (ToolResult, error) {
			l, _ := spawnLinkFrom(runCtx)
			s.deps.Tracker.setOneShot(parent, l.id, &oneShotRecord{Mode: spawnModeRaw})
			keepNote(t, s, runCtx, "held note")
			writeText(runCtx, "half an answer")
			close(wrote)
			<-runCtx.Done()
			return ToolResult{}, runCtx.Err()
		})
		ret <- err
	}()
	waitRunning(t, s, parent)
	<-wrote
	s.deps.Tracker.BeginShutdown()
	cancel()
	recv(t, ret, "runDirect's return")
	noInject(t, got, 100*time.Millisecond)
}
