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

// blocksSpawnAgent runs one turn that streams blocks as text blocks, with a
// tool call after the first, and completes with final: a delegated backend's
// turn, whose final text joins every message of the turn.
type blocksSpawnAgent struct {
	blocks []string
	final  string
}

func (a *blocksSpawnAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	for i, b := range a.blocks {
		turnevent.Emit(ctx, turnevent.TextBlock{Text: b, Phase: turnevent.PhaseIntermediate})
		if i == 0 {
			turnevent.Emit(ctx, turnevent.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command": "true"}`)})
		}
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: a.final})
	return nil
}

// endsInMarker reports whether text ends in a silencing marker.
func endsInMarker(text string) bool {
	return platform.StripSilencingSuffix(text) != strings.TrimSpace(text)
}

// The work Mac's repro (#3238): a note before a tool call, then two messages
// of only [[NO_RESPONSE]].
var silentRunBlocks = []string{"Now the todo item.", "[[NO_RESPONSE]]", "[[NO_RESPONSE]]"}

const silentRunFinal = "Now the todo item.\n\n[[NO_RESPONSE]]\n\n[[NO_RESPONSE]]"

// TestSpawnResult_SilentLastMessageIsNotTheAnswer is #3238: a background
// spawn's run whose last message is only [[NO_RESPONSE]] reports the text
// before it, without the markers and with a note that it may be progress
// notes, not the bare markers.
func TestSpawnResult_SilentLastMessageIsNotTheAnswer(t *testing.T) {
	t.Parallel()
	h := newFollowupHarness(&blocksSpawnAgent{blocks: silentRunBlocks, final: silentRunFinal})
	h.start(t, "do the work")
	msg := <-h.delivered
	if endsInMarker(msg) {
		t.Errorf("result ends in [[NO_RESPONSE]]:\n%s", msg)
	}
	for _, want := range []string{"Now the todo item.", spawnSilentLastNote} {
		if !strings.Contains(msg, want) {
			t.Errorf("result lacks %q:\n%s", want, msg)
		}
	}
}

// TestSpawnResult_OnlyMarkersSaysNoAnswer: a run whose whole text is
// [[NO_RESPONSE]] reports that it gave no answer.
func TestSpawnResult_OnlyMarkersSaysNoAnswer(t *testing.T) {
	t.Parallel()
	h := newFollowupHarness(&blocksSpawnAgent{blocks: []string{"[[NO_RESPONSE]]"}, final: "[[NO_RESPONSE]]"})
	h.start(t, "do the work")
	msg := <-h.delivered
	if !strings.Contains(msg, spawnNoAnswer) {
		t.Errorf("result = %q, want it to say the spawn gave no answer", msg)
	}
}

// TestSpawnResult_MarkerAfterRealReplyIsRemoved: a marker added to the end
// of a message with real text is removed, and the rest is the answer, with
// no note.
func TestSpawnResult_MarkerAfterRealReplyIsRemoved(t *testing.T) {
	t.Parallel()
	h := newFollowupHarness(&blocksSpawnAgent{blocks: []string{"working", "the report. [[NO_RESPONSE]]"}, final: "working\n\nthe report. [[NO_RESPONSE]]"})
	h.start(t, "do the work")
	msg := <-h.delivered
	if endsInMarker(msg) || !strings.HasSuffix(msg, "the report.") {
		t.Errorf("result = %q, want it to end with the report, without the marker", msg)
	}
	if strings.Contains(msg, spawnSilentLastNote) {
		t.Errorf("result carries the progress-notes note though its last message has real text:\n%s", msg)
	}
}

// TestSpawnBranchTurn_SilentRunIsNotTheAnswer is #3238 for a run the
// spawn's backend started by itself (a background task it started
// finished): the case where a spawn most often answers [[NO_RESPONSE]].
func TestSpawnBranchTurn_SilentRunIsNotTheAnswer(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAutonomousHarness(t, "auto3238a", agent)
	h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.resultFor(t, parent)

	sink, ok := SpawnBranchTurn(parent+"/b1000000000", func() {})
	if !ok {
		t.Fatal("SpawnBranchTurn did not claim the spawn's branch")
	}
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	for _, b := range silentRunBlocks {
		sink.Emit(ctx, turnevent.TextBlock{Text: b, Phase: turnevent.PhaseIntermediate})
	}
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: silentRunFinal})

	msg := h.resultFor(t, parent)
	if endsInMarker(msg) || !strings.Contains(msg, spawnSilentLastNote) {
		t.Errorf("autonomous run's result = %q, want the text before the marker and the note", msg)
	}
}

func TestSpawnAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, final string
		blocks      []string
		want        string
	}{
		{"plain answer", "done", []string{"done"}, "done"},
		{"empty stays empty", "", nil, ""},
		{"only a marker", "[[NO_RESPONSE]]", []string{"[[NO_RESPONSE]]"}, spawnNoAnswer},
		{"wrapped marker", "`[[NO_RESPONSE]]`", nil, spawnNoAnswer},
		{"API answer with marker added", "the report.\n\n[[NO_RESPONSE]]", nil, "the report."},
		{"last message silent", silentRunFinal, silentRunBlocks, "Now the todo item.\n\n" + spawnSilentLastNote},
		{"marker mid-text kept", "use [[NO_RESPONSE]] to stay quiet", nil, "use [[NO_RESPONSE]] to stay quiet"},
	} {
		if got := spawnAnswer(tc.final, tc.blocks); got != tc.want {
			t.Errorf("%s: spawnAnswer(%q) = %q, want %q", tc.name, tc.final, got, tc.want)
		}
	}
}

// sessionSendAgent runs each turn by calling send_to_session to target on
// its own session, then answering [[NO_RESPONSE]], as the work Mac's spawn
// did (#3238).
type sessionSendAgent struct {
	tool   *Tool
	target string
	mu     sync.Mutex
	result []string
}

func (a *sessionSendAgent) HandleMessage(ctx context.Context, sk string, _ []string, _ []platform.Attachment) error {
	params, _ := json.Marshal(map[string]string{"session_key": a.target, "message": "the report: PRs merged", "reply_to": "session"})
	res, err := a.tool.Execute(WithSessionKey(ctx, sk), params)
	a.mu.Lock()
	if err != nil {
		a.result = append(a.result, "error: "+err.Error())
	} else {
		a.result = append(a.result, res.Text)
	}
	a.mu.Unlock()
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "[[NO_RESPONSE]]"})
	return nil
}

func (a *sessionSendAgent) results() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.result...)
}

// TestSendToSessionToSpawnerFollowsHeldRoute is #3238's repro: a background
// spawn whose spawner (a depth leaf) has reported sends its report to that
// spawner with send_to_session, then ends with [[NO_RESPONSE]]. The report
// goes as send_to_spawner: kept while the route holds, and at the head of the
// result its new owner receives.
func TestSendToSessionToSpawnerFollowsHeldRoute(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	agent := &sessionSendAgent{tool: NewSendToSessionTool(&mockSessionAppender{}, notifier, nil, nil, nil, nil), target: "sss3238/imain"}
	cb := &characterBranches{}
	var mu sync.Mutex
	hold := true
	s := NewSpawner(SpawnDeps{AgentID: "sss3238", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker(), CharacterBranch: cb.create,
		Route: func(p string) SpawnRoute {
			mu.Lock()
			defer mu.Unlock()
			if hold {
				return SpawnRoute{Owner: p, Hold: true}
			}
			return SpawnRoute{Owner: "sss3238/iroot", Via: p}
		}},
		func() SpawnAgent { return agent })
	registerTestSpawner(t, s)
	ctx := WithSessionKey(context.Background(), "sss3238/imain")
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"name":"report1","prompt":"job","background":true}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(s.deps.Tracker.heldResults("sss3238")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the result was never held; deliveries so far: %d; tool said %q", len(got), agent.results())
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	hold = false
	mu.Unlock()
	s.Reown()
	m := recv(t, got, "the released result")
	if m.session != "sss3238/iroot" {
		t.Errorf("result went to %q, want the new owner sss3238/iroot", m.session)
	}
	for _, want := range []string{"[MESSAGES FROM YOUR SPAWN]", "the report: PRs merged", spawnNoAnswer} {
		if !strings.Contains(m.msg, want) {
			t.Errorf("result lacks %q:\n%s", want, m.msg)
		}
	}
	if r := agent.results(); len(r) != 1 || !strings.Contains(r[0], "went as send_to_spawner") || !strings.Contains(r[0], "Kept:") {
		t.Errorf("tool said %q, want it sent as send_to_spawner and kept", r)
	}
}

// TestSendToSessionFromSpawnToOtherSessionUnchanged: a background spawn's
// send_to_session to a session that is not its spawner is delivered as
// before.
func TestSendToSessionFromSpawnToOtherSessionUnchanged(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	agent := &sessionSendAgent{tool: NewSendToSessionTool(&mockSessionAppender{}, notifier, nil, nil, nil, nil), target: "sso3238/iother"}
	cb := &characterBranches{}
	s := NewSpawner(SpawnDeps{AgentID: "sso3238", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker(), CharacterBranch: cb.create},
		func() SpawnAgent { return agent })
	registerTestSpawner(t, s)
	ctx := WithSessionKey(context.Background(), "sso3238/imain")
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"name":"other1","prompt":"job","background":true}`)); err != nil {
		t.Fatal(err)
	}
	recv(t, got, "the [SPAWN RESULT]")
	if r := agent.results(); len(r) != 1 || !strings.HasPrefix(r[0], "Message sent to session sso3238/iother") {
		t.Errorf("tool said %q, want an ordinary send to sso3238/iother", r)
	}
}
