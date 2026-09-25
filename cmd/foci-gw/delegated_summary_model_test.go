package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
)

// summaryModelBackend records the StartOptions of each session it launches;
// everything else is inert. The batch turn is stubbed, so nothing here ever
// has to answer a turn.
type summaryModelBackend struct {
	mu      sync.Mutex
	started []delegator.StartOptions
}

var _ delegator.Delegator = (*summaryModelBackend)(nil)

func (b *summaryModelBackend) Start(_ context.Context, opts delegator.StartOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = append(b.started, opts)
	return nil
}
func (b *summaryModelBackend) ImmediateInject(context.Context, delegator.Inject) error { return nil }
func (b *summaryModelBackend) WaitForTurn(context.Context) error                       { return nil }
func (b *summaryModelBackend) IsTurnInFlight() bool                                    { return false }
func (b *summaryModelBackend) IsRunning() bool                                         { return true }
func (b *summaryModelBackend) SetPermissionPromptFunc(delegator.PermissionPromptFunc)  {}
func (b *summaryModelBackend) SetOnPromptsCleared(func())                              {}
func (b *summaryModelBackend) RegisterPromptCancelListener(string, func(string))       {}
func (b *summaryModelBackend) SetOnSessionReady(func(string))                          {}
func (b *summaryModelBackend) SetTypingFunc(func(bool))                                {}
func (b *summaryModelBackend) AttachSessionEvents(*delegator.SessionEvents)            {}
func (b *summaryModelBackend) SendKeystroke(context.Context, string) error             { return nil }
func (b *summaryModelBackend) SendSpecialKey(context.Context, string) error            { return nil }
func (b *summaryModelBackend) Interrupt(context.Context) error                         { return nil }
func (b *summaryModelBackend) SessionID() string                                       { return "" }
func (b *summaryModelBackend) SessionFilePath() string                                 { return "" }
func (b *summaryModelBackend) WaitReady(context.Context) error                         { return nil }
func (b *summaryModelBackend) CheckReady(context.Context) (bool, error)                { return true, nil }
func (b *summaryModelBackend) StatusDetail() string                                    { return "" }
func (b *summaryModelBackend) Close() error                                            { return nil }

func (b *summaryModelBackend) models() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, o := range b.started {
		out = append(out, o.Model)
	}
	return out
}

// cheapSummaryModelBackend is a backend with a cheap model of its own, as
// ccstream has (haiku).
type cheapSummaryModelBackend struct{ summaryModelBackend }

func (*cheapSummaryModelBackend) BatchCheapModel() string { return "haiku" }

type startRecorder interface {
	delegator.Delegator
	models() []string
}

// runDelegatedSummary drives foci_summary through the production delegated
// wiring (buildExecRegistry) into a real DelegatedManager, whose session
// launch is where the batch model is finally chosen. The turn itself is
// stubbed: it only has to launch the batch session.
func runDelegatedSummary(t *testing.T, be startRecorder, agentModel, summaryModel string) []string {
	t.Helper()
	ws := t.TempDir()
	p := minimalSetupParams(t, "olly")
	p.acfg.Workspace = ws
	p.acfg.Backend = "opencode"
	resolved := &config.ResolvedAgentConfig{}
	resolved.Summary.SummaryModel = summaryModel
	p.resolved = resolved
	p.resolvedLive = config.NewLiveValue(resolved)

	mgr := &agent.DelegatedManager{
		AgentID:    "olly",
		StartOpts:  delegator.StartOptions{AgentID: "olly", WorkDir: ws, Model: agentModel},
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
	}
	mgr.RunBatchTurn = func(ctx context.Context, sessionKey, _, _ string) (string, error) {
		if _, err := mgr.Get(ctx, sessionKey); err != nil {
			return "", err
		}
		return "a summary", nil
	}
	ag := &agent.Agent{AgentID: "olly", DelegatedManager: mgr}

	registry := buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return ag })
	tool := registry.Get("summary")
	if tool == nil {
		t.Fatal("delegated registry has no summary tool")
	}
	file := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(file, []byte("some content worth summarising\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]string{"file": file, "prompt": "what is this?"})
	res, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if res.Text != "a summary" {
		t.Fatalf("summary returned %q, want the batch turn's text", res.Text)
	}
	return be.models()
}

// TestDelegatedSummary_OpencodeModelIsResolvable is the #2032 regression: an
// opencode agent's foci_summary batch launched with the literal "haiku" — a
// Claude Code alias opencode cannot resolve, so it silently fell back to the
// opencode server's own default model. With no cheap model of its own and
// none configured, the batch must run on the agent's own model.
func TestDelegatedSummary_OpencodeModelIsResolvable(t *testing.T) {
	got := runDelegatedSummary(t, &summaryModelBackend{}, "zai-coding-plan/glm-4.6", "")
	if len(got) != 1 || got[0] != "zai-coding-plan/glm-4.6" {
		t.Fatalf("opencode summary batch launched with models %q, want the agent's own [zai-coding-plan/glm-4.6]", got)
	}
}

// TestDelegatedSummary_BackendCheapModel: a backend with a cheap model of its
// own (CC: haiku) runs summaries on it.
func TestDelegatedSummary_BackendCheapModel(t *testing.T) {
	got := runDelegatedSummary(t, &cheapSummaryModelBackend{}, "opus", "")
	if len(got) != 1 || got[0] != "haiku" {
		t.Fatalf("summary batch launched with models %q, want the backend's cheap model [haiku]", got)
	}
}

// TestDelegatedSummary_ConfiguredModelWins: [tools] summary_model overrides
// both the backend's cheap model and the agent's model.
func TestDelegatedSummary_ConfiguredModelWins(t *testing.T) {
	got := runDelegatedSummary(t, &cheapSummaryModelBackend{}, "opus", "sonnet")
	if len(got) != 1 || got[0] != "sonnet" {
		t.Fatalf("summary batch launched with models %q, want the configured [sonnet]", got)
	}
}
