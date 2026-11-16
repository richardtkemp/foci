package agent

import (
	"context"
	"sync"
	"testing"

	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/turnevent"
)

// spawnRecSink records what reaches a spawn's sink (SpawnBranchTurn's).
type spawnRecSink struct {
	mu       sync.Mutex
	started  int
	complete []turnevent.TurnComplete
}

func (s *spawnRecSink) Emit(_ context.Context, ev turnevent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := ev.(type) {
	case turnevent.TurnStart:
		s.started++
	case turnevent.TurnComplete:
		s.complete = append(s.complete, e)
	}
}
func (s *spawnRecSink) DeliversToPlatform() bool { return false }

// #3143 D2: a turn on a background spawn's branch that would deliver to a
// chat (an injection: an async tool result, a wake) reports to the spawn's
// spawner instead. pg-logs' final report went to the human's most recent
// conversation as an "unsolicited send to unbound session".
// Fail-arm: without the spawnBranchTurnCtx call in HandleMessage the reply
// reaches conn.
func TestHandleMessage_SpawnBranchTurnReportsToSpawner(t *testing.T) {
	t.Parallel()
	ag := newSentinelTestAgent(t, "the logs are clean")
	const sk = "test/ispawnbranch"
	spawn := &spawnRecSink{}
	ag.SpawnBranchTurn = func(key string, _ func()) (turnevent.Sink, bool) {
		if key != sk {
			return nil, false
		}
		return spawn, true
	}

	conn := &recordingConn{}
	ctx := turnevent.WithSink(context.Background(), turn.NewSessionSink(conn, sk, "test"))
	if err := ag.HandleMessage(ctx, sk, []string{"[async result] the grep finished"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(conn.sent) != 0 {
		t.Errorf("the spawn branch's turn reached a chat: %q", conn.sent)
	}
	spawn.mu.Lock()
	defer spawn.mu.Unlock()
	if spawn.started != 1 || len(spawn.complete) != 1 || spawn.complete[0].FinalText != "the logs are clean" {
		t.Errorf("spawn sink got %d start(s), completions %+v; want one turn with the answer", spawn.started, spawn.complete)
	}
}

// The spawn's own turns (its spawner's sink does not deliver to a platform)
// and any session that is not a spawn's branch are left as they are.
func TestHandleMessage_SpawnBranchTurnLeavesOthers(t *testing.T) {
	t.Parallel()
	const sk = "test/iother"
	claimed := 0
	claim := func(string, func()) (turnevent.Sink, bool) { claimed++; return nil, false }

	ag := newSentinelTestAgent(t, "hello")
	ag.SpawnBranchTurn = claim
	conn := &recordingConn{}
	ctx := turnevent.WithSink(context.Background(), turn.NewSessionSink(conn, sk, "test"))
	if err := ag.HandleMessage(ctx, sk, []string{"hi"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(conn.sent) != 1 {
		t.Errorf("a turn on a session no spawn runs on delivered %q, want its reply", conn.sent)
	}

	own := newSentinelTestAgent(t, "spawn's own answer")
	own.SpawnBranchTurn = func(string, func()) (turnevent.Sink, bool) {
		t.Error("the spawn's own (non-delivering) turn was offered to SpawnBranchTurn")
		return nil, false
	}
	buf := turnevent.NewBufferSink()
	if err := own.HandleMessage(turnevent.WithSink(context.Background(), buf), sk, []string{"go"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if buf.FinalText() != "spawn's own answer" {
		t.Errorf("the spawn's own sink got %q", buf.FinalText())
	}
}

// #3143 D2a: no compaction-resume nudge on a background spawn's branch: its
// spawner owns how it goes on. Fail-arm: the nudge was injected (pg-logs'
// was held through the hold and ran at the lift).
func TestMaybeInjectCompactionResume_SpawnBranchSkipped(t *testing.T) {
	t.Parallel()
	const parent, branch = "test/c3143", "test/c3143/b3143"
	trackSpawnBranch(t, parent, branch, false)
	var mu sync.Mutex
	var got []string
	a := &Agent{AsyncNotifier: tools.NewAsyncNotifier(func(_ string, msg, _, _ string, _ *tools.AgentOrigin) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg)
	})}
	a.maybeInjectCompactionResume(context.Background(), branch)
	a.maybeInjectCompactionResume(context.Background(), parent)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("injections = %d (%q), want only the parent's", len(got), got)
	}
}
