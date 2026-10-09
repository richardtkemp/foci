package main

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// liveModelBackend is an inert Delegator standing in for a Claude Code
// process that has named its exact model (system/init carried
// "claude-opus-5-5") but has not completed a turn yet. Its LiveModel method
// makes it a delegator.LiveModelReporter; everything else is inert because
// the test never drives a turn — only the whoami read path may run.
type liveModelBackend struct{}

var _ delegator.Delegator = (*liveModelBackend)(nil)

func (b *liveModelBackend) Start(context.Context, delegator.StartOptions) error                { return nil }
func (b *liveModelBackend) ImmediateInject(context.Context, delegator.Inject) error            { return nil }
func (b *liveModelBackend) WaitForTurn(context.Context) error                                  { return nil }
func (b *liveModelBackend) IsTurnInFlight() bool                                               { return false }
func (b *liveModelBackend) IsRunning() bool                                                    { return true }
func (b *liveModelBackend) SetPermissionPromptFunc(delegator.PermissionPromptFunc)             {}
func (b *liveModelBackend) SetOnPromptsCleared(func())                                         {}
func (b *liveModelBackend) RegisterPromptCancelListener(string, func(string))                  {}
func (b *liveModelBackend) SetOnSessionReady(func(string))                                     {}
func (b *liveModelBackend) SetTypingFunc(func(bool))                                           {}
func (b *liveModelBackend) AttachSessionEvents(*delegator.SessionEvents)                       {}
func (b *liveModelBackend) Interrupt(context.Context) error                                    { return nil }
func (b *liveModelBackend) SessionID() string                                                  { return "" }
func (b *liveModelBackend) SessionFilePath() string                                            { return "" }
func (b *liveModelBackend) WaitReady(context.Context) error                                    { return nil }
func (b *liveModelBackend) CheckReady(context.Context) (bool, error)                           { return true, nil }
func (b *liveModelBackend) StatusDetail() string                                               { return "" }
func (b *liveModelBackend) Close() error                                                       { return nil }
func (b *liveModelBackend) LiveModel() string                                                  { return "claude/claude-opus-5-5" }

// TestBuildExecRegistryWhoamiReportsBackendModel is the #2252 wiring lock:
// whoami must go through the production tool_table row (buildExecRegistry)
// to Agent.ReportedSessionModel, so mid-first-turn it reports the model the
// session's RUNNING backend already learned — here "claude/claude-opus-5-5"
// — not the configured alias "opus" that Agent.SessionModel would return
// before the first turn completes. The manager is seeded through Get (the
// one sanctioned creator) so the whoami lookup itself exercises only the
// non-creating read path.
func TestBuildExecRegistryWhoamiReportsBackendModel(t *testing.T) {
	t.Parallel()

	p := minimalSetupParams(t, "olly")
	p.acfg.Backend = "claude-code"

	mgr := &agent.DelegatedManager{
		AgentID:    "olly",
		StartOpts:  delegator.StartOptions{AgentID: "olly", WorkDir: t.TempDir()},
		NewBackend: func() (delegator.Delegator, error) { return &liveModelBackend{}, nil },
	}
	ag := &agent.Agent{AgentID: "olly", Model: "opus", DelegatedManager: mgr}

	registry := buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return ag })
	tool := registry.Get("whoami")
	if tool == nil {
		t.Fatal("registry missing whoami tool")
	}

	// The session's backend is running and has named its model; its first
	// turn has NOT completed, so the session has no model of its own.
	if _, err := mgr.Get(context.Background(), "olly/c77"); err != nil {
		t.Fatalf("seed the session's running backend: %v", err)
	}

	res, err := tool.Execute(tools.WithSessionKey(context.Background(), "olly/c77"), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	want := "agent_id: olly\n" +
		"session_key: olly/c77\n" +
		"chat_id: 77\n" +
		"platform: unknown\n" + // nil sessionIndex
		"backend: claude-code\n" +
		"transport: delegated\n" +
		"model: claude/claude-opus-5-5\n" // the backend's live model, not the "opus" alias
	if res.Text != want {
		t.Errorf("whoami =\n%s\nwant\n%s", res.Text, want)
	}
}
