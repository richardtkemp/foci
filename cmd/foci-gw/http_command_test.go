package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
)

// TestCommand_HappyPath proves POST /command dispatches a registered slash
// command through the agent's command registry and returns its result: 200
// with the command's text, and the agent backend never called (commands run
// outside the agent loop).
func TestCommand_HappyPath(t *testing.T) {
	ping := &command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	}
	d, mock := httpTestSetup(t, httpTestOpts{commands: []*command.Command{ping}})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/ping"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "pong" {
		t.Errorf("response = %q, want pong", resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) for a command, want 0", len(calls))
	}
}

// TestCommand_Unknown pins the behaviour for an unregistered command name:
// the registry answers unknown names itself (with an "Unknown command"
// suggestion message, found=true), so the endpoint returns 200 with that
// text rather than the handler's 404 branch — which only fires for a
// registered command with no Execute function.
func TestCommand_Unknown(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/nosuchcommand"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["response"], "Unknown command") {
		t.Errorf("response = %q, want an Unknown command message", resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) for an unknown command, want 0", len(calls))
	}
}

// TestCommand_IfInactive proves POST /command honours the activity gate: a
// command carrying if_inactive is skipped when the targeted session ran a turn
// within the window. This is the wiring behind the overnight-reset cron
// (`foci command --if-inactive 55m -a <agent> /reset`) — the gate must stop the
// reset from firing on a session that is still active or mid-turn.
//
// Mirrors TestWebhook_IfInactive: same stubConnMgr session base and same
// last_activity seeding via session_metadata.
func TestCommand_IfInactive(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})

	// Recent session activity → an --if-inactive command must skip.
	d.sessionIndex.SetSessionMetadata(testSessionKey, "last_activity", fmt.Sprintf("%d", time.Now().Unix()))

	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"agent":"test-agent","command":"/reset","if_inactive":"1h"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: session recently active" {
		t.Errorf("response = %q, want skip message (gate not wired into /command?)", resp["response"])
	}
}
