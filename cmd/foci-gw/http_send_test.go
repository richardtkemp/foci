package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
)

// postJSON posts body to path on mux and returns the recorder.
func postJSON(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestSend_SyncHappyPath proves the /send sync path end-to-end: 200, the
// response body carries the mock backend's reply, and the routing receipt
// names the resolved session and the ladder rung that matched (empty selector
// → the agent's default session via rung "default").
func TestSend_SyncHappyPath(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hello there"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != mockReply {
		t.Errorf("response = %q, want %q", resp["response"], mockReply)
	}
	if resp["session"] != testSessionKey {
		t.Errorf("session = %q, want %q", resp["session"], testSessionKey)
	}
	if resp["resolved_via"] != "default" {
		t.Errorf("resolved_via = %q, want %q", resp["resolved_via"], "default")
	}
	// The turn pipeline prepends a [meta] header; the request text is the tail.
	if got := mock.lastText(); !strings.HasSuffix(got, "\n\nhello there") {
		t.Errorf("backend saw %q, want text ending in %q", got, "hello there")
	}
	if calls := mock.snapshot(); len(calls) != 1 || calls[0].trigger != "user" {
		t.Errorf("calls = %+v, want one call with trigger \"user\"", calls)
	}
}

// TestSend_Async proves the async path: 202 with a "queued" status and the
// routing receipt returned immediately, and the queued turn still executes
// afterwards — the 202 is an acceptance, not a completed turn, so the test
// waits for the mock backend to see the text.
func TestSend_Async(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"do it later","async":true}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "queued" {
		t.Errorf("status = %q, want queued", resp["status"])
	}
	if resp["session"] != testSessionKey || resp["resolved_via"] != "default" {
		t.Errorf("receipt = session %q via %q, want %q via default", resp["session"], resp["resolved_via"], testSessionKey)
	}

	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "\n\ndo it later") {
			t.Errorf("async turn text = %q, want text ending in %q", text, "do it later")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("202 returned but the queued turn never reached the backend")
	}
}

// TestSend_SlashCommand proves a /send whose text starts with "/" dispatches
// through the agent's command registry instead of the agent loop: the command
// result comes back with the routing receipt and the backend is never called.
func TestSend_SlashCommand(t *testing.T) {
	ping := &command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	}
	d, mock := httpTestSetup(t, httpTestOpts{commands: []*command.Command{ping}})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"/ping"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "pong" {
		t.Errorf("response = %q, want pong", resp["response"])
	}
	if resp["session"] != testSessionKey {
		t.Errorf("session = %q, want %q", resp["session"], testSessionKey)
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) for a slash command, want 0: %+v", len(calls), calls)
	}
}

// TestSend_SerialisesBehindInFlightTurn is the HTTP-level regression test for
// "system input never steers running work" (the reason /send routes through
// runAgentQueued → the session inbox worker instead of driving the turn on the
// request goroutine). A first sync /send blocks inside the mock backend; a
// second sync /send to the same session must NOT reach the backend while the
// first is in flight — it queues, runs only after the first is released, and
// the backend sees the two texts in order, never interleaved.
func TestSend_SerialisesBehindInFlightTurn(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string)   // unbuffered: receive == a turn is inside the backend
	mock.proceed = make(chan struct{}) // each turn blocks until released
	mux := newTestMux(d)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postJSON(mux, "/send", `{"text":"first"}`) }()

	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "\n\nfirst") {
			t.Fatalf("first turn entered backend with %q, want text ending in first", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never reached the backend")
	}

	// First turn is now blocked inside the backend. Fire the second send.
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- postJSON(mux, "/send", `{"text":"second"}`) }()

	// The second turn must not reach the backend (or complete) while the
	// first holds the session — that would be steering running work.
	select {
	case text := <-mock.entered:
		t.Fatalf("second turn (%q) entered the backend while the first was still in flight", text)
	case w := <-secondDone:
		t.Fatalf("second /send completed (%d: %s) while the first turn was still in flight", w.Code, w.Body.String())
	case <-time.After(200 * time.Millisecond):
		// Serialised: still queued behind the in-flight turn.
	}

	// Release the first turn; the second must then run to completion.
	mock.proceed <- struct{}{}
	if w := <-firstDone; w.Code != http.StatusOK {
		t.Fatalf("first /send status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "\n\nsecond") {
			t.Fatalf("second turn entered backend with %q, want text ending in second", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second turn never reached the backend after the first was released")
	}
	mock.proceed <- struct{}{}
	if w := <-secondDone; w.Code != http.StatusOK {
		t.Fatalf("second /send status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	calls := mock.snapshot()
	if len(calls) != 2 || !strings.HasSuffix(calls[0].text, "first") || !strings.HasSuffix(calls[1].text, "second") {
		t.Errorf("backend calls = %+v, want [first, second] in order", calls)
	}
}
