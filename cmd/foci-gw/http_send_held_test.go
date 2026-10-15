package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/route"
)

// postSendAsync issues an async /send against the harness and returns the
// decoded 202 receipt.
func postSendAsync(t *testing.T, d httpHandlerDeps) map[string]string {
	t.Helper()
	w := postJSON(newTestMux(d), "/send", `{"text":"ping","async":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("/send async status = %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	var rcpt map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &rcpt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	return rcpt
}

// TestSend_Async_HeldField pins requirement 6: when the target session is
// held behind background work at the moment the async /send is accepted, the
// 202 receipt says so via a held field — the message is queued all the same.
func TestSend_Async_HeldField(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})

	// An adopted delivering run in flight (the worker idle) holds the
	// session's system input.
	d.agents[testAgentID].ag.SetInFlightDeliveringForTest(testSessionKey, true)
	t.Cleanup(func() { d.agents[testAgentID].ag.SetInFlightDeliveringForTest(testSessionKey, false) })

	rcpt := postSendAsync(t, d)
	if rcpt["held"] != "background_work" {
		t.Fatalf("receipt = %v, want held=background_work while the session is held behind background work", rcpt)
	}
	if rcpt["status"] != "queued" {
		t.Errorf("status = %q, want queued (the message is still accepted)", rcpt["status"])
	}
}

// TestSend_Async_NoHeldFieldWhenNotHeld is the characterisation pin: with no
// hold the 202 receipt is exactly today's — no held field at all.
func TestSend_Async_NoHeldFieldWhenNotHeld(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})

	rcpt := postSendAsync(t, d)
	if _, present := rcpt["held"]; present {
		t.Fatalf("receipt = %v, want no held field when nothing holds the session", rcpt)
	}
	if rcpt["status"] != "queued" {
		t.Errorf("status = %q, want queued", rcpt["status"])
	}
}

// TestWriteAccepted_HeldOmittedWhenEmpty pins the receipt writer directly:
// an empty held reason produces byte-identical output to the old form, which
// is what the branch and choice-carrying (user envelope) paths pass.
func TestWriteAccepted_HeldOmittedWhenEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	writeAccepted(w, route.Receipt{Target: testAgentID, SessionKey: testSessionKey, Via: route.RungExact}, "")
	if strings.Contains(w.Body.String(), "held") {
		t.Fatalf("receipt body = %q, want no held key for an empty reason", w.Body.String())
	}
	for _, want := range []string{`"status":"queued"`, `"session"`, `"resolved_via"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("receipt body = %q, missing %s", w.Body.String(), want)
		}
	}
}
