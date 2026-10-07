package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/defersend"
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
// cache-touch seeding via session_index.last_cache_touch.
func TestCommand_IfInactive(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})

	// Recent session activity → an --if-inactive command must skip.
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())

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

// resetSpy registers a /reset test command that records every execution, so a
// test can prove a deferred command was NOT dispatched (and later, that the
// sweep dispatches it).
func resetSpy() (*command.Command, *atomic.Int32) {
	var runs atomic.Int32
	return &command.Command{
		Name: "reset",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			runs.Add(1)
			return command.Response{Text: "reset done"}, nil
		},
	}, &runs
}

// TestCommand_UnmetWaitDeferred proves /command honours the wait gates
// (#1272): a wait condition that does not hold now (evaluated against the
// session the command targets) stores the request in the deferred queue —
// HTTP 202 with the deferred receipt, nothing dispatched — instead of
// skipping and hoping the next cron tick lands.
func TestCommand_UnmetWaitDeferred(t *testing.T) {
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm → wait_cold unmet
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/reset","wait_cold":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Errorf("status=%v want deferred; body=%s", resp["status"], w.Body.String())
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("command executed %d time(s) despite the unmet wait, want 0", n)
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("queued=%d want 1", len(all))
	}
	r := all[0]
	if r.Kind != defersend.KindCommand {
		t.Errorf("Kind=%q want %q", r.Kind, defersend.KindCommand)
	}
	if r.Text != "/reset" {
		t.Errorf("Text=%q want the command string %q", r.Text, "/reset")
	}
	if r.SessionKey != testSessionKey {
		t.Errorf("SessionKey=%q want the default session %q", r.SessionKey, testSessionKey)
	}
}

// TestCommand_WaitHoldsRunsNow is the holds-now half of #1272's command wait
// gates, and pins that /command gets NO default gate: a request with no wait
// field, or one whose condition already holds (or wait_none), dispatches
// immediately exactly as before — nothing is queued.
func TestCommand_WaitHoldsRunsNow(t *testing.T) {
	cases := []struct {
		name string
		warm bool
		body string
	}{
		{"no wait field runs now (no default gate)", false, `{"command":"/reset"}`},
		{"met condition runs now", false, `{"command":"/reset","wait_cold":"1m"}`},
		{"wait_none runs now even when unmet", true, `{"command":"/reset","wait_cold":"1h","wait_none":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy, runs := resetSpy()
			d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
			store := withDeferStore(t, &d)
			if tc.warm {
				d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())
			}
			mux := newTestMux(d)

			w := postJSON(mux, "/command", tc.body)

			if w.Code != http.StatusOK {
				t.Fatalf("code=%d want 200 (dispatched now); body=%s", w.Code, w.Body.String())
			}
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["response"] != "reset done" {
				t.Errorf("response=%q want the command's reply", resp["response"])
			}
			if n := runs.Load(); n != 1 {
				t.Errorf("command executed %d time(s), want 1", n)
			}
			if all, _ := store.All(); len(all) != 0 {
				t.Errorf("queued=%d want 0", len(all))
			}
		})
	}
}

// TestCommand_BadWaitDuration proves a malformed wait duration on /command is
// a client error, not a dispatch.
func TestCommand_BadWaitDuration(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/reset","wait_cold":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", w.Code, w.Body.String())
	}
}
