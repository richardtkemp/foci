package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// openChatRecorder stands in for app.OpenSessionConversation and records the
// session keys /send asked to open a chat for.
type openChatRecorder struct {
	mu   sync.Mutex
	keys []string
	err  error
}

func (r *openChatRecorder) open(agentID, sessionKey string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, agentID+"|"+sessionKey)
	return r.err == nil, r.err
}

func (r *openChatRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// TestSend_NewNamedSession_OpensAppChat proves #3097: a /send whose session
// selector names a session that does not exist opens an app chat for the new
// named session (agent/i<name>) unless the request asks for background-only,
// and opens none for an existing session, the default session, or a send an
// --if-* gate skips.
func TestSend_NewNamedSession_OpensAppChat(t *testing.T) {
	t.Parallel()
	newKey := testAgentID + "/inewthing"
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"new name opens a chat", `{"text":"hi","session":"newthing","wait_none":true}`, []string{testAgentID + "|" + newKey}},
		{"background keeps it out of the app", `{"text":"hi","session":"newthing","wait_none":true,"background":true}`, nil},
		{"default session opens nothing", `{"text":"hi","wait_none":true}`, nil},
		{"existing session key opens nothing", `{"text":"hi","session":"` + testSessionKey + `","wait_none":true}`, nil},
		{"a skipped send opens nothing", `{"text":"hi","session":"newthing","if_active":"1h"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := httpTestSetup(t, httpTestOpts{})
			rec := &openChatRecorder{}
			d.openSessionChat = rec.open
			mux := newTestMux(d)

			w := postJSON(mux, "/send", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			got := rec.calls()
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("openSessionChat calls = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSend_NewNamedSession_OpenFailureStillSends proves a failed chat open
// (an agent with no app connection) does not refuse the send: the new
// session runs in the background, as before #3097.
func TestSend_NewNamedSession_OpenFailureStillSends(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	rec := &openChatRecorder{err: errors.New("agent has no app connection")}
	d.openSessionChat = rec.open
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","session":"newthing","wait_none":true,"async":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if len(rec.calls()) != 1 {
		t.Errorf("openSessionChat calls = %v, want one", rec.calls())
	}
	if calls := mock.snapshot(); len(calls) == 0 {
		t.Error("backend was not called: a failed chat open must not stop the send")
	}
}

// TestSend_NewNamedSession_ReadableName proves a session name with spaces,
// punctuation and an emoji survives every gateway layer (#3097): the selector
// resolves to a NEW named session whose key carries the name verbatim, the app
// chat is opened for exactly that key, the message reaches the backend, and
// the session's transcript file is written under the name. A second send to
// the same name finds the existing session (named rung) and opens no second
// chat.
func TestSend_NewNamedSession_ReadableName(t *testing.T) {
	t.Parallel()
	const name = "Tax 2026 Q&A 🥔"
	wantKey := testAgentID + "/i" + name
	d, mock := httpTestSetup(t, httpTestOpts{})
	rec := &openChatRecorder{}
	d.openSessionChat = rec.open
	mux := newTestMux(d)

	body, _ := json.Marshal(map[string]any{"text": "hello there", "session": name, "wait_none": true})
	w := postJSON(mux, "/send", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, w.Body.String())
	}
	if resp["session"] != wantKey || resp["resolved_via"] != "created" {
		t.Fatalf("receipt = %v, want session %q via created", resp, wantKey)
	}
	if got := rec.calls(); len(got) != 1 || got[0] != testAgentID+"|"+wantKey {
		t.Errorf("openSessionChat calls = %q, want one for %q", got, wantKey)
	}
	if got := mock.lastText(); !strings.HasSuffix(got, "\n\nhello there") {
		t.Errorf("backend saw %q, want text ending in %q", got, "hello there")
	}
	path, err := d.sessions.SessionPath(wantKey)
	if err != nil {
		t.Fatalf("SessionPath(%q): %v", wantKey, err)
	}
	if !strings.Contains(path, name) {
		t.Errorf("session path %q does not carry the name", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("session transcript not written: %v", err)
	}
	if !d.sessionIndex.SessionExists(wantKey) {
		t.Errorf("session index has no row for %q", wantKey)
	}

	w = postJSON(mux, "/send", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("second send: status = %d; body: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["session"] != wantKey || resp["resolved_via"] != "named" {
		t.Errorf("second receipt = %v, want session %q via named", resp, wantKey)
	}
	if got := rec.calls(); len(got) != 1 {
		t.Errorf("openSessionChat calls after second send = %q, want still one", got)
	}
}
