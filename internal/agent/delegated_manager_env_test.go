package agent

import (
	"context"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestGet_SessionEnvIsBackendAgnostic pins WHERE the CC-only
// BASH_MAX_TIMEOUT_MS injection lives. DelegatedManager builds one
// per-session env (FOCI_SESSION_KEY, FOCI_SOCK, BASH_ENV) that EVERY
// delegated backend receives — ccstream, codex and opencode alike —
// so a Claude Code-specific var placed here would reach all of them. The var
// is therefore injected inside ccstream's own buildEnv
// (internal/delegator/ccstream/env.go) instead, and this test fails if it
// ever migrates up to the shared layer.
//
// The api backend needs no counterpart test: an API agent has no
// DelegatedManager and spawns no subprocess at all.
func TestGet_SessionEnvIsBackendAgnostic(t *testing.T) {
	t.Parallel()
	mgr, mocks := newTestManager(t, nil)

	if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(*mocks) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(*mocks))
	}

	be := (*mocks)[0]
	be.mu.Lock()
	env := be.startOpts.Env
	be.mu.Unlock()

	// Sanity: the manager really did build a per-session env.
	if env["FOCI_SESSION_KEY"] != "test-agent/c1" {
		t.Fatalf("FOCI_SESSION_KEY = %q, want %q", env["FOCI_SESSION_KEY"], "test-agent/c1")
	}
	for _, k := range []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS", "CLAUDE_CODE_MAX_RETRIES", "CLAUDE_CODE_RETRY_WATCHDOG"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s=%q in the shared per-session env — it would reach codex/opencode too; keep it in ccstream", k, v)
		}
	}
}

// TestWantsPromptSuggestions pins who pays for a prediction (#2916): only a root
// chat session, where someone can read it.
func TestWantsPromptSuggestions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key   string
		batch bool
		want  bool
	}{
		{"clutch/c123", false, true},
		{"clutch/c123", true, false},
		{"clutch/c123/b1709596800", false, false},
		{"clutch/c123/i1709596801", false, false},
		{"clutch/iresearch", false, false},
		{"not a key", false, false},
	}
	for _, c := range cases {
		if got := wantsPromptSuggestions(c.key, c.batch); got != c.want {
			t.Errorf("wantsPromptSuggestions(%q, batch=%v) = %v, want %v", c.key, c.batch, got, c.want)
		}
	}
}

func TestGet_RootChatLaunchesWithPromptSuggestions(t *testing.T) {
	t.Parallel()
	mgr, mocks := newTestManager(t, nil)
	if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	be := (*mocks)[0]
	be.mu.Lock()
	defer be.mu.Unlock()
	if !be.startOpts.PromptSuggestions {
		t.Fatal("a root chat session launched without PromptSuggestions")
	}
}

// wantsPromptSuggestion pins requirement 1 and the first half of 2 of #2922: only
// a user's input on a session with somewhere to show it asks for a prediction, and
// the surface is read each time, so a session that moves to the app needs nothing.
func TestWantsPromptSuggestion(t *testing.T) {
	a := &Agent{}
	if a.wantsPromptSuggestion(true, "a/c1") {
		t.Error("no surface wired: want false")
	}
	bound := false
	a.SetPromptSuggestionSurface(func(sk string) bool { return sk == "a/c1" && bound })
	if a.wantsPromptSuggestion(true, "a/c1") {
		t.Error("user input with no surface yet: want false")
	}
	bound = true
	if !a.wantsPromptSuggestion(true, "a/c1") {
		t.Error("user input after the session moved to the app: want true")
	}
	if a.wantsPromptSuggestion(false, "a/c1") {
		t.Error("a turn nobody started on a surface: want false")
	}
	if a.wantsPromptSuggestion(true, "a/c2") {
		t.Error("user input on another session with no surface: want false")
	}
}

// A silent turn's prediction must not reach the chip; a reply lifts that.
func TestPromptSuggestion_SilentTurnDropped(t *testing.T) {
	a := &Agent{}
	var got []string
	a.SetOnPromptSuggestion(func(_, text string) { got = append(got, text) })
	be := &mockBackendDT{}
	a.AttachDelivery(be, "a/c1")
	se := be.sessionEvents

	for _, silent := range []string{"[[NO_RESPONSE]]", "  `[[NO_RESPONSE]]` ", ""} {
		a.noteTurnReply("a/c1", silent)
		se.OnPromptSuggestion("after silence")
		if len(got) != 0 {
			t.Fatalf("after final text %q: delivered %q, want nothing", silent, got)
		}
	}
	a.noteTurnReply("a/c1", "a real reply")
	se.OnPromptSuggestion("after a reply")
	if len(got) != 1 || got[0] != "after a reply" {
		t.Fatalf("after a reply: delivered %q, want the suggestion", got)
	}
}

// RunInference carries the decision on the inject that begins the turn: the user
// on a surface asks, a turn nobody started does not, and neither does a user with
// no surface. An unattended turn and a cron turn are the system path.
func TestRunInference_PromptSuggestionFollowsTurn(t *testing.T) {
	RegisterPlatformTrigger("app")
	RegisterPlatformTrigger("telegram")
	cases := []struct {
		name    string
		trigger string
		user    bool
		surface bool
		want    bool
	}{
		{"app user", "app", false, true, true},
		{"ask answer", "ask_grader", true, true, true},
		{"telegram user, no surface", "telegram", false, false, false},
		{"cron send", "external", false, true, false},
		{"keepalive", TriggerKeepalive, false, true, false},
		{"reflection", "reflection", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
			a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}
			a.SetPromptSuggestionSurface(func(string) bool { return c.surface })
			ctx := WithTrigger(context.Background(), c.trigger)
			if c.user {
				ctx = WithUserInput(ctx)
			}
			ts := NewTurnState(ctx, "test/s", []string{"x"}, nil)
			ts.Trigger = c.trigger
			ts.Prompt = "x"
			ts.StartedAt = time.Now()
			if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
				t.Fatalf("RunInference: %v", err)
			}
			injects := be.InjectsRecorded()
			if len(injects) != 1 {
				t.Fatalf("injects = %d, want 1", len(injects))
			}
			if injects[0].PromptSuggestion != c.want {
				t.Errorf("PromptSuggestion = %v, want %v", injects[0].PromptSuggestion, c.want)
			}
		})
	}
}

// The turn's own completion is what keeps a silent turn's prediction off the chip.
func TestRunInference_SilentReplyHoldsBackSuggestion(t *testing.T) {
	RegisterPlatformTrigger("app")
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}
	var got []string
	a.SetOnPromptSuggestion(func(_, text string) { got = append(got, text) })
	a.AttachDelivery(be, "test/s")

	run := func(reply string) {
		t.Helper()
		ts := NewTurnState(WithTrigger(context.Background(), "app"), "test/s", []string{"x"}, nil)
		ts.Trigger = "app"
		ts.Prompt = "x"
		ts.StartedAt = time.Now()
		if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
			t.Fatalf("RunInference: %v", err)
		}
		injects := be.InjectsRecorded()
		injects[len(injects)-1].Turn.OnTurnComplete(&delegator.TurnResult{Text: reply})
	}

	run("[[NO_RESPONSE]]")
	be.sessionEvents.OnPromptSuggestion("after silence")
	if len(got) != 0 {
		t.Fatalf("after a silent turn: delivered %q", got)
	}
	run("a reply")
	be.sessionEvents.OnPromptSuggestion("after a reply")
	if len(got) != 1 {
		t.Fatalf("after a reply: delivered %q, want one", got)
	}
}

// Only the user's input takes down a shown prediction: a turn nobody started must
// leave the chip alone (#2924). The surface is not part of this: a chip left from
// before the session moved stays out of date either way.
func TestRunInference_UserInputClearsSuggestion(t *testing.T) {
	RegisterPlatformTrigger("app")
	RegisterPlatformTrigger("telegram")
	cases := []struct {
		name    string
		trigger string
		user    bool
		want    int
	}{
		{"app user", "app", false, 1},
		{"ask answer", "ask_grader", true, 1},
		{"telegram user", "telegram", false, 1},
		{"cron send", "external", false, 0},
		{"keepalive", TriggerKeepalive, false, 0},
		{"reflection", "reflection", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
			a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}
			got := 0
			a.SetOnPromptSuggestionStale(func(sk string) {
				if sk == "test/s" {
					got++
				}
			})
			ctx := WithTrigger(context.Background(), c.trigger)
			if c.user {
				ctx = WithUserInput(ctx)
			}
			ts := NewTurnState(ctx, "test/s", []string{"x"}, nil)
			ts.Trigger = c.trigger
			ts.Prompt = "x"
			ts.StartedAt = time.Now()
			if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
				t.Fatalf("RunInference: %v", err)
			}
			if got != c.want {
				t.Errorf("stale calls = %d, want %d", got, c.want)
			}
		})
	}
}
