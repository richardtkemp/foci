package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/defersend"
	"foci/internal/session"
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

// sessionKeySpy registers a /probe command that records the session key of
// every execution (command.Request.SessionKey), so a test can assert WHICH
// session a /command dispatch landed on — the observable behind every
// session-selector test below. The mutex keeps it race-clean if a test ever
// drives the deferred sweep concurrently.
func sessionKeySpy() (*command.Command, func() []string) {
	var mu sync.Mutex
	var keys []string
	return &command.Command{
			Name: "probe",
			Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
				mu.Lock()
				keys = append(keys, req.SessionKey)
				mu.Unlock()
				return command.Response{Text: "probed"}, nil
			},
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), keys...)
		}
}

// TestCommand_SessionNamedDispatchesThere is the #2296 red test: a /command
// whose body names an existing named session dispatches the command on THAT
// session's key, not the agent's default — the probe command sees the named
// key, and the response is the command's own reply.
func TestCommand_SessionNamedDispatchesThere(t *testing.T) {
	probe, keys := sessionKeySpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
	registerRoot(t, d, testAgentID+"/iside")
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/probe","session":"side"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("#2296: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "probed" {
		t.Errorf("#2296: response = %q, want the command's reply", resp["response"])
	}
	if got := keys(); len(got) != 1 || got[0] != testAgentID+"/iside" {
		t.Errorf("#2296: command ran on session key(s) %v, want exactly %q — the selector was ignored", got, testAgentID+"/iside")
	}
}

// TestCommand_SessionGateEvaluatedAgainstTarget pins that the activity gate
// consults the session the SELECTOR resolved to, not the agent's default:
// if_inactive must dispatch while the named target is cold even with a warm
// default, and skip while the named target is warm even with a cold default.
// On the base code both arms evaluate the default and get it wrong in
// opposite directions.
func TestCommand_SessionGateEvaluatedAgainstTarget(t *testing.T) {
	t.Run("warm default, cold target dispatches", func(t *testing.T) {
		probe, keys := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		registerRoot(t, d, testAgentID+"/iside")
		d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // default warm, target cold
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe","session":"side","if_inactive":"1h"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("#2296: status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["response"] != "probed" {
			t.Errorf("#2296: response = %q, want the command's reply (the gate consulted the warm DEFAULT, not the cold target)", resp["response"])
		}
		if got := keys(); len(got) != 1 || got[0] != testAgentID+"/iside" {
			t.Errorf("#2296: command ran on %v, want %q", got, testAgentID+"/iside")
		}
	})

	t.Run("cold default, warm target skips", func(t *testing.T) {
		probe, keys := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		registerRoot(t, d, testAgentID+"/iside")
		d.sessionIndex.TouchCacheTouch(testAgentID+"/iside", time.Now()) // target warm, default cold
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe","session":"side","if_inactive":"1h"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("#2296: status = %d, want 200 (a skip answers 200); body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["response"] != "skipped: session recently active" {
			t.Errorf("#2296: response = %q, want the skip message (the gate must see the warm TARGET, not the cold default)", resp["response"])
		}
		if got := keys(); len(got) != 0 {
			t.Errorf("#2296: command ran %d time(s) on %v despite the warm target", len(got), got)
		}
	})
}

// TestCommand_SessionUnknownNameCreatesKey pins the #2284 create rung for
// /command (#2296): a selector that matches no key, named session or alias
// is a valid NAME — the command dispatches on the created <agent>/i<name>
// key, the same rule /send and /branch resolve by.
func TestCommand_SessionUnknownNameCreatesKey(t *testing.T) {
	probe, keys := sessionKeySpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/probe","session":"nosuch"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("#2296: status = %d, want 200 (an unknown name creates the session); body: %s", w.Code, w.Body.String())
	}
	if got := keys(); len(got) != 1 || got[0] != testAgentID+"/inosuch" {
		t.Errorf("#2296: command ran on %v, want the created key %q", got, testAgentID+"/inosuch")
	}
}

// TestCommand_SessionSelectorParamRefused pins that /command's selector
// carries the same embedded-parameter grammar /branch's does: ?create=false
// on an otherwise-unknown name is refused with the SAME 400 body /branch
// answers (the resolver's create-disabled error), not dispatched on the
// default session.
func TestCommand_SessionSelectorParamRefused(t *testing.T) {
	probe, keys := sessionKeySpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/probe","session":"nosuch?create=false"}`)
	branch := postJSON(mux, "/branch", `{"text":"q","session":"nosuch?create=false"}`)

	if w.Code != http.StatusBadRequest || branch.Code != http.StatusBadRequest {
		t.Fatalf("#2296: /command=%d /branch=%d, want both 400; bodies: %q / %q", w.Code, branch.Code, w.Body.String(), branch.Body.String())
	}
	if w.Body.String() != branch.Body.String() {
		t.Errorf("#2296: /command refusal %q differs from /branch's %q — the selector must resolve identically", w.Body.String(), branch.Body.String())
	}
	if got := keys(); len(got) != 0 {
		t.Errorf("#2296: command ran %d time(s) on %v despite the refused selector", len(got), got)
	}
}

// TestCommand_NoSessionStillDefault characterises the empty-selector rule:
// with no session field, /command keeps dispatching on the agent's default
// session key directly (defaultSessionKey, no CreateDefault minting) — the
// pre-#2296 behaviour, unchanged. Both halves are pinned directly: dispatch
// on the default key, and a counting createDefault hook that must stay at
// zero in a harness where minting is the only way a session could appear.
func TestCommand_NoSessionStillDefault(t *testing.T) {
	t.Run("dispatches on the default key", func(t *testing.T) {
		probe, keys := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if got := keys(); len(got) != 1 || got[0] != testSessionKey {
			t.Errorf("command ran on %v, want the default session %q", got, testSessionKey)
		}
	})

	t.Run("never mints a default conversation", func(t *testing.T) {
		probe, keys := sessionKeySpy()
		// noSession: the agent has no session a default rung could find, so
		// the counting hook is the only way /command could produce a key.
		d, _ := httpTestSetup(t, httpTestOpts{noSession: true, commands: []*command.Command{probe}})
		var mints atomic.Int32
		d.createDefault = func(agentID string) (string, error) {
			mints.Add(1)
			return session.NewChatSessionKey(agentID, 99), nil
		}
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (an empty default key is dispatched on, not refused); body: %s", w.Code, w.Body.String())
		}
		if n := mints.Load(); n != 0 {
			t.Errorf("createDefault called %d time(s) — a selector-less /command must never mint a conversation (#1859)", n)
		}
		if got := keys(); len(got) != 1 || got[0] != "" {
			t.Errorf("command ran on %v, want the plain defaultSessionKey result (empty here)", got)
		}
	})
}

// TestCommand_DeferredWithSessionStoresResolvedKey is the deferred arm of
// #2296: a /command with a selector and an unmet wait (evaluated against the
// named target) stores the RESOLVED key in the defer record — so the sweep
// dispatches on the named session — and the 202 receipt names the resolved
// session and rung from the resolution, not a bare default-key receipt.
func TestCommand_DeferredWithSessionStoresResolvedKey(t *testing.T) {
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	registerRoot(t, d, testAgentID+"/iside")
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testAgentID+"/iside", time.Now()) // target warm → wait_cold unmet
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/reset","session":"side","wait_cold":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("#2296: status = %d, want 202 (the TARGET is warm, so wait_cold defers); body: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Errorf("status field = %v, want deferred; body: %s", resp["status"], w.Body.String())
	}
	if resp["session"] != testAgentID+"/iside" {
		t.Errorf("#2296: receipt session = %v, want the resolved %q", resp["session"], testAgentID+"/iside")
	}
	if resp["resolved_via"] != "named" {
		t.Errorf("#2296: receipt resolved_via = %v, want named (the resolution's rung)", resp["resolved_via"])
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("queued = %d, want 1", len(all))
	}
	if all[0].Kind != defersend.KindCommand || all[0].SessionKey != testAgentID+"/iside" {
		t.Errorf("#2296: record = %+v, want one command on the resolved key %q", all[0], testAgentID+"/iside")
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("command executed %d time(s) despite the unmet wait, want 0", n)
	}
}

// TestCommand_ReceiptInImmediateResponse pins the receipt half of #2296's
// selector ruling: an immediate /command answers with the routing receipt
// ({response, target, session, resolved_via}) — the same envelope /send's
// slash-command path uses — so the caller can SEE where the command ran,
// including the resolved_via "created" of a mistyped selector, which without
// the receipt would run /reset on a brand-new empty session unseen. With no
// selector the receipt names the default key with rung "default".
func TestCommand_ReceiptInImmediateResponse(t *testing.T) {
	t.Run("named selector", func(t *testing.T) {
		probe, _ := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		registerRoot(t, d, testAgentID+"/iside")
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe","session":"side"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("#2296: status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["response"] != "probed" {
			t.Errorf("#2296: response = %q, want the command's reply", resp["response"])
		}
		if resp["session"] != testAgentID+"/iside" {
			t.Errorf("#2296: receipt session = %q, want %q — the response must say where the command ran", resp["session"], testAgentID+"/iside")
		}
		if resp["resolved_via"] != "named" {
			t.Errorf("#2296: receipt resolved_via = %q, want named", resp["resolved_via"])
		}
	})

	t.Run("mistyped selector says created", func(t *testing.T) {
		probe, _ := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe","session":"reserch"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("#2296: status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["session"] != testAgentID+"/ireserch" {
			t.Errorf("#2296: receipt session = %q, want the created %q", resp["session"], testAgentID+"/ireserch")
		}
		if resp["resolved_via"] != "created" {
			t.Errorf("#2296: receipt resolved_via = %q, want created — a typo must be visible, not a silent fresh session", resp["resolved_via"])
		}
	})

	t.Run("no selector names the default key", func(t *testing.T) {
		probe, _ := sessionKeySpy()
		d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
		mux := newTestMux(d)

		w := postJSON(mux, "/command", `{"command":"/probe"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("#2296: status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["session"] != testSessionKey {
			t.Errorf("#2296: receipt session = %q, want the default %q", resp["session"], testSessionKey)
		}
		if resp["target"] != testAgentID {
			t.Errorf("#2296: receipt target = %q, want the agent %q — the resolver's canonical target, like /send's empty selector", resp["target"], testAgentID)
		}
		if resp["resolved_via"] != "default" {
			t.Errorf("#2296: receipt resolved_via = %q, want default (the plain default key is that rung's result)", resp["resolved_via"])
		}
	})
}

// TestCommand_NoSelectorReceiptMatchesSend pins that a selector-less
// /command answers with EXACTLY /send's receipt for the same empty selector:
// both arms build the receipt from the resolver's own result, so target,
// session and resolved_via must match field for field — never from a
// hand-assembled receipt that drops the canonical target.
func TestCommand_NoSelectorReceiptMatchesSend(t *testing.T) {
	probe, _ := sessionKeySpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{probe}})
	mux := newTestMux(d)

	cmdW := postJSON(mux, "/command", `{"command":"/probe"}`)
	sendW := postJSON(mux, "/send", `{"text":"/probe"}`)

	if cmdW.Code != http.StatusOK || sendW.Code != http.StatusOK {
		t.Fatalf("status /command=%d /send=%d, want 200/200; bodies: %q / %q", cmdW.Code, sendW.Code, cmdW.Body.String(), sendW.Body.String())
	}
	var cmdResp, sendResp map[string]string
	_ = json.Unmarshal(cmdW.Body.Bytes(), &cmdResp)
	_ = json.Unmarshal(sendW.Body.Bytes(), &sendResp)
	if sendResp["target"] != testAgentID {
		t.Fatalf("/send receipt target = %q, want %q — the comparison baseline is broken", sendResp["target"], testAgentID)
	}
	for _, field := range []string{"target", "session", "resolved_via"} {
		if cmdResp[field] != sendResp[field] {
			t.Errorf("receipt %s: /command=%q /send=%q — a selector-less /command must answer /send's receipt for the same empty selector", field, cmdResp[field], sendResp[field])
		}
	}
}

// TestCommand_DecodeErrorNamesField pins the #2296 decode-error contract: a
// body that fails to decode ("human" as a string where the struct wants a
// bool) answers 400 naming the decode problem — encoding/json's error names
// the field and type — instead of the "need {...}" hint that misreports a
// mistyped field as a missing one.
func TestCommand_DecodeErrorNamesField(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/ping","human":"yes"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("#2296: status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"bad request:", "cannot unmarshal", "human"} {
		if !strings.Contains(body, want) {
			t.Errorf("#2296: decode-error body %q does not name %q", body, want)
		}
	}
	if strings.Contains(body, "need {") {
		t.Errorf("#2296: decode-error body %q claims a missing field — a mistyped field must be named as the decode problem", body)
	}
}

// TestCommand_EmptyBodyStillNeedText characterises the kept half of the
// decode split: a body that decodes CLEANLY but leaves the required field
// empty still answers the "need {...}" hint.
func TestCommand_EmptyBodyStillNeedText(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `need {"command": "/ping"}`) {
		t.Errorf("body = %q, want the kept need-command hint", body)
	}
}

// TestCommand_BodyTooLarge413 characterises the unchanged cap: an over-cap
// /command body answers 413, not a 400 decode message.
func TestCommand_BodyTooLarge413(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)
	big := strings.Repeat("a", (1<<20)+1024) // just over the 1 MiB cap

	w := postJSON(mux, "/command", `{"command":"/`+big+`"}`)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}
