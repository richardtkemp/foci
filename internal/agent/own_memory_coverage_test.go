package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"foci/internal/delegator"
	"foci/internal/memsearch"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turnevent"
	"foci/internal/workspace"
)

// Coverage of the own-material index feed (#3187, audit before any refactor).
// The index must see the tool input and output of every session of a family,
// whatever kind of turn runs the tools and whichever sink that turn has. Each
// test below drives one kind of session end to end (a real turn on a mock
// backend or provider, not a call to the feed itself) and checks that the
// lines reached the index under the family's key. They pin the feed's
// coverage, not its call sites: a change of where the index is fed must keep
// every one of them green.

const (
	// ownSubWritten is a line a subagent's tool wrote.
	ownSubWritten = "- A subagent wrote this line into a memory file on the parent's behalf."
)

// ownToolInput is a Bash tool input that writes ownWritten to a memory file.
var ownToolInput = mustJSON(map[string]string{"command": "cat >> memory/x.md <<'EOF'\n" + ownWritten + "\nEOF"})

// ownSubToolInput is a subagent Write tool input with ownSubWritten.
var ownSubToolInput = mustJSON(map[string]string{"file_path": "memory/y.md", "content": ownSubWritten + "\n"})

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// emitOwnToolEvents plays one tool call with its result, and one subagent
// tool call, through the session events, as a delegated backend does.
func emitOwnToolEvents(se *delegator.SessionEvents) {
	se.OnToolStart("toolu_own", "Bash", ownToolInput)
	se.OnToolEnd("toolu_own", "Bash", ownRead, false)
	se.OnSubagentTool("toolu_sub", "Write", ownSubToolInput)
}

// assertOwnLines fails unless the main tool's input and output, and (when
// sub) the subagent tool's input, are in family's window.
func assertOwnLines(t *testing.T, x *memsearch.Index, family string, sub bool) {
	t.Helper()
	if !ownLineIn(t, x, family, ownWritten) {
		t.Errorf("tool input not in %s's own material", family)
	}
	if !ownLineIn(t, x, family, ownRead) {
		t.Errorf("tool output not in %s's own material", family)
	}
	if sub && !ownLineIn(t, x, family, ownSubWritten) {
		t.Errorf("subagent tool input not in %s's own material", family)
	}
}

// ownDelegatedAgent is a delegated agent (one mock backend for every
// session, as newBatchTestAgent) with an own-material index. Each turn plays
// emitOwnToolEvents once the turn's sink is registered on the session's
// router (*emitKey, set by the caller before the turn), then completes.
func ownDelegatedAgent(t *testing.T, emitKey *atomic.Value) (*Agent, *batchTurnBackend, *memsearch.Index) {
	t.Helper()
	x := openOwnIndex(t)
	be := &batchTurnBackend{}
	be.sessionFile = "/tmp/own-coverage.jsonl"
	var a *Agent
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		be.mu.Lock()
		se := be.sessionEvents
		be.mu.Unlock()
		sk, _ := emitKey.Load().(string)
		go func() {
			defer h.OnTurnComplete(&delegator.TurnResult{Text: "done", Model: "claude-sonnet-4-5"})
			if !waitRegistered(t, a, sk) {
				return
			}
			emitOwnToolEvents(se)
		}()
		return nil, nil
	}
	a = newBatchTestAgent(t, be)
	a.OwnMemory = x
	return a, be, x
}

// TestOwnMemoryCoverage_DelegatedTurnKinds: on a delegated backend
// (claude-code, codex, opencode all deliver through the same session events)
// every kind of turn feeds the index: the system turns the periodic runner
// and the memory hooks start (no sink on ctx), ordinary injected turns, and a
// spawn's turn on its branch (a BufferSink on ctx), whose lines count for the
// parent's family.
func TestOwnMemoryCoverage_DelegatedTurnKinds(t *testing.T) {
	const root = "helen/c42"
	const branch = "helen/c42/b1795000000"
	for _, c := range []struct {
		name, trigger, key string
		sink               func() turnevent.Sink
	}{
		{"cron", "cron", root, nil},
		{"reflection", "reflection", branch, nil},
		{"session_end_memory", "session_end_memory", branch, nil},
		{"compaction_memory", "compaction_memory", branch, nil},
		{"keepalive", "keepalive", root, nil},
		{"background", "background", branch, nil},
		{"branch", "branch", branch, nil},
		{"spawn", "", branch, func() turnevent.Sink { return turnevent.NewBufferSink() }},
		{"injected-delivering", "agent", root, func() turnevent.Sink { return fnSink(func(context.Context, turnevent.Event) {}) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var key atomic.Value
			key.Store(c.key)
			a, _, x := ownDelegatedAgent(t, &key)
			ctx := WithTrigger(context.Background(), c.trigger)
			if c.sink != nil {
				ctx = turnevent.WithSink(ctx, c.sink())
			}
			if err := a.HandleMessage(ctx, c.key, []string{"go"}, nil); err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			assertOwnLines(t, x, root, true)
		})
	}
}

// TestOwnMemoryCoverage_DelegatedBatchTurns: a batch turn (consolidation,
// the utility purposes, a model-mode spawn) runs on a fresh batch key under
// its owner; its tools count for the owner's family.
func TestOwnMemoryCoverage_DelegatedBatchTurns(t *testing.T) {
	const owner = "helen/c42"
	for _, purpose := range []string{
		delegator.BatchPurposeConsolidation,
		delegator.BatchPurposeNudgeExtraction,
		delegator.BatchPurposeSummary,
		delegator.BatchPurposePromptDiff,
		delegator.BatchPurposeSpawnCharacter,
	} {
		t.Run(purpose, func(t *testing.T) {
			var key atomic.Value
			key.Store("")
			a, _, x := ownDelegatedAgent(t, &key)
			if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "go", OwnerSessionKey: owner, Purpose: purpose,
				OnSession: func(batchKey string) { key.Store(batchKey) },
			}); err != nil {
				t.Fatalf("RunBatch: %v", err)
			}
			assertOwnLines(t, x, owner, true)
		})
	}
}

// TestOwnMemoryCoverage_DelegatedPlatformTurn: a platform message's turn,
// driven through the session inbox with the platform's own streaming sink.
func TestOwnMemoryCoverage_DelegatedPlatformTurn(t *testing.T) {
	const sk = "helen/c42"
	var key atomic.Value
	key.Store(sk)
	a, _, x := ownDelegatedAgent(t, &key)
	userTurnThroughInbox(t, a, sk)
	assertOwnLines(t, x, sk, true)
}

// TestOwnMemoryCoverage_DelegatedAutonomousRun: a run the backend starts by
// itself (CC reacting to a background task's result), adopted by foci.
func TestOwnMemoryCoverage_DelegatedAutonomousRun(t *testing.T) {
	const sk = "helen/c42"
	x := openOwnIndex(t)
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/own-coverage-auto.jsonl"
	be.setTurnInFlight(true)
	a := newBatchTestAgent(t, be)
	a.OwnMemory = x
	a.ResolveLateConn = func(string) platform.Connection { return &leakConn{} }
	a.AttachDelivery(be, sk)
	a.OpenAutonomousTurn(sk, be, nil)
	if !waitRegistered(t, a, sk) {
		return
	}
	emitOwnToolEvents(be.events())
	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
	assertOwnLines(t, x, sk, true)
}

// TestOwnMemoryCoverage_DelegatedBetweenTurns: tool events that arrive while
// no turn of the session is running (a background task's tail, a late hook)
// reach the router's late-delivery fallback, with and without a chat to
// deliver to. A session with no chat (a background session, a spawn after
// its run) is the case a delivery-side feed would miss.
func TestOwnMemoryCoverage_DelegatedBetweenTurns(t *testing.T) {
	for _, c := range []struct {
		name string
		conn platform.Connection
	}{
		{"with-chat", &leakConn{}},
		{"no-chat", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			const sk = "helen/c42"
			x := openOwnIndex(t)
			be := &mockBackendDT{}
			a := &Agent{AgentID: "helen", OwnMemory: x}
			a.ResolveLateConn = func(string) platform.Connection { return c.conn }
			a.AttachDelivery(be, sk)
			if a.IsTurnInFlight(sk) || a.sessionRouter(sk).current.Load() != nil {
				t.Fatal("precondition: a turn is registered")
			}
			emitOwnToolEvents(be.sessionEvents)
			assertOwnLines(t, x, sk, true)
		})
	}
}

// ownAPIAgent is an API-transport agent with one tool, "write", whose result
// is ownRead, and a provider that calls it once with ownToolInput and then
// ends the turn.
func ownAPIAgent(t *testing.T) (*Agent, *memsearch.Index) {
	t.Helper()
	x := openOwnIndex(t)
	var calls atomic.Int32
	client := newTestClient(func(*provider.MessageRequest) *provider.MessageResponse {
		if calls.Add(1)%2 == 1 {
			return &provider.MessageResponse{
				ID: "msg_tool", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{{
					Type: "tool_use", ID: "tu_own", Name: "write", Input: json.RawMessage(ownToolInput),
				}},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_end", Type: "message", Role: "assistant",
			Content: provider.TextContent("done"), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name: "write", Description: "w", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult(ownRead), nil
		},
	})
	a := &Agent{
		AgentID:        "helen",
		Client:         client,
		Sessions:       session.NewStore(t.TempDir()),
		Tools:          registry,
		Bootstrap:      workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:          "claude-haiku-4-5",
		OwnMemory:      x,
		injectGatePoll: testGatePoll,
	}
	return a, x
}

// TestOwnMemoryCoverage_APITurnKinds: on the API transport every kind of
// turn feeds the index from its tool loop, the system turns (no sink on ctx)
// and a spawn's turn (a BufferSink) included. API agents have no subagents
// of their own (a spawn is a session of its own, covered here).
func TestOwnMemoryCoverage_APITurnKinds(t *testing.T) {
	const root = "helen/c42"
	const branch = "helen/c42/b1795000000"
	for _, c := range []struct {
		name, trigger, key string
		sink               func() turnevent.Sink
	}{
		{"cron", "cron", root, nil},
		{"reflection", "reflection", branch, nil},
		{"session_end_memory", "session_end_memory", branch, nil},
		{"compaction_memory", "compaction_memory", branch, nil},
		{"keepalive", "keepalive", root, nil},
		{"background", "background", branch, nil},
		{"consolidation", delegator.BatchPurposeConsolidation, branch, nil},
		{"nudge_extraction", delegator.BatchPurposeNudgeExtraction, branch, nil},
		{"branch", "branch", branch, nil},
		{"spawn", "", branch, func() turnevent.Sink { return turnevent.NewBufferSink() }},
		{"injected-delivering", "agent", root, func() turnevent.Sink { return fnSink(func(context.Context, turnevent.Event) {}) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, x := ownAPIAgent(t)
			ctx := WithTrigger(context.Background(), c.trigger)
			if c.sink != nil {
				ctx = turnevent.WithSink(ctx, c.sink())
			}
			if err := a.HandleMessage(ctx, c.key, []string{"go"}, nil); err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			assertOwnLines(t, x, root, false)
		})
	}
}

// TestOwnMemoryCoverage_APIPlatformTurn: a platform message's turn on the
// API transport, through the session inbox with the platform's sink.
func TestOwnMemoryCoverage_APIPlatformTurn(t *testing.T) {
	const sk = "helen/c42"
	a, x := ownAPIAgent(t)
	userTurnThroughInbox(t, a, sk)
	assertOwnLines(t, x, sk, false)
}
