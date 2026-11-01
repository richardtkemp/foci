package slack

// fake_slack_test.go: an in-process fake of the two Slack surfaces the
// transport talks to — the Web API (httptest server) and Socket Mode (a
// local WebSocket). No test ever reaches slack.com.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeCall is one recorded Web API call: the method (path element after
// /api/), the decoded JSON body, and the Authorization header's token shape.
type fakeCall struct {
	Method string
	Body   map[string]any
	Auth   string // the raw Authorization header value
}

// fakeSlack serves the Web API and a Socket Mode endpoint, records every
// call, and lets tests push envelopes to the (single) socket client.
type fakeSlack struct {
	t     *testing.T
	srv   *httptest.Server
	httpc *http.Client

	mu       sync.Mutex
	calls    []fakeCall
	acks     []string // envelope ids acked, in order
	upgrader websocket.Upgrader

	sockMu    sync.Mutex
	sockConn  *websocket.Conn // the one connected socket client, if any
	sockReady chan struct{}   // closed when a socket client connected

	// FailAuth makes auth.test (and only it) fail with the given error code.
	FailAuth string
	// FailSocketOpen makes apps.connections.open fail with the given code.
	FailSocketOpen string
	// SocketURLOverride, when set, is the URL apps.connections.open hands
	// out instead of the fake's own socket URL (a malformed URL proves the
	// dial error carries no session ticket).
	SocketURLOverride string
	// SocketOpenRawBody, when set, is written verbatim as the
	// apps.connections.open response body (an oversized body proves the
	// response read is capped).
	SocketOpenRawBody []byte
	// quietUnknown suppresses the unknown-method test failure (the
	// self-test triggers one on purpose).
	quietUnknown bool
}

// newFakeSlack starts the fake; Close stops it.
func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{t: t, sockReady: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = httptest.NewServer(mux)
	f.httpc = f.srv.Client()
	t.Cleanup(f.srv.Close)
	return f
}

// apiBase is the Web API base the transport config should point at.
func (f *fakeSlack) apiBase() string { return f.srv.URL + "/api" }

// socketURL is the ws URL handed out by apps.connections.open.
func (f *fakeSlack) socketURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/socket"
}

func (f *fakeSlack) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.URL.Path == "/socket" {
		f.serveSocket(w, r)
		return
	}
	auth := r.Header.Get("Authorization")

	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: path, Body: body, Auth: auth})
	f.mu.Unlock()

	respond := func(payload map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
	switch path {
	case "auth.test":
		if f.FailAuth != "" {
			respond(map[string]any{"ok": false, "error": f.FailAuth})
			return
		}
		respond(map[string]any{"ok": true, "user_id": "UBOTID"})
	case "apps.connections.open":
		if f.SocketOpenRawBody != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(f.SocketOpenRawBody)
			return
		}
		if f.FailSocketOpen != "" {
			respond(map[string]any{"ok": false, "error": f.FailSocketOpen})
			return
		}
		url := f.socketURL()
		if f.SocketURLOverride != "" {
			url = f.SocketURLOverride
		}
		respond(map[string]any{"ok": true, "url": url})
	case "chat.postMessage":
		respond(map[string]any{"ok": true, "ts": fmt.Sprintf("%s.000001", bodyTS(body))})
	case "chat.update":
		respond(map[string]any{"ok": true})
	case "chat.delete":
		respond(map[string]any{"ok": true})
	case "files.getUploadURLExternal":
		respond(map[string]any{"ok": true, "upload_url": f.srv.URL + "/upload", "file_id": "F123"})
	case "/upload", "upload":
		w.WriteHeader(http.StatusOK)
	case "files.completeUploadExternal":
		// Slack makes "files" a required array of {id, title}; a body
		// without it is rejected with missing_argument. The fake mirrors
		// that so a client-side shape regression fails here instead of
		// passing as a silently-completed upload.
		if files, ok := body["files"].([]any); !ok || len(files) == 0 {
			respond(map[string]any{"ok": false, "error": "missing_argument"})
			return
		}
		respond(map[string]any{"ok": true})
	default:
		// An unknown method is a client bug (a typo would otherwise pass
		// silently): record it, answer with a loud error code, and fail the
		// test unless it was triggered on purpose.
		if !f.quietUnknown {
			f.t.Errorf("fake slack: unknown method %q called", path)
		}
		respond(map[string]any{"ok": false, "error": "unknown_method_" + path})
	}
}

// bodyTS digs a deterministic ts out of the request body (chat.postMessage
// tests assert on it via the recorded body, so only uniqueness matters).
func bodyTS(body map[string]any) string {
	if ch, ok := body["channel"].(string); ok && ch != "" {
		return fmt.Sprintf("%09d", len(ch)+int(sumLen(ch)))
	}
	return "100000000"
}

func sumLen(s string) int64 {
	var n int64
	for _, r := range s {
		n += int64(r)
	}
	return n
}

// serveSocket upgrades the Socket Mode client and hands it to pushers.
func (f *fakeSlack) serveSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("fake slack: socket upgrade: %v", err)
		return
	}
	f.sockMu.Lock()
	f.sockConn = conn
	f.sockMu.Unlock()
	close(f.sockReady)
	// The socket client never writes beyond acks; nothing to read here, but
	// drain so half-closes do not wedge. Acks are read by push's waiters via
	// the recorded list, so start a reader.
	go func() {
		for {
			var ack map[string]any
			if err := conn.ReadJSON(&ack); err != nil {
				return
			}
			if id, ok := ack["envelope_id"].(string); ok {
				f.mu.Lock()
				f.acks = append(f.acks, id)
				f.mu.Unlock()
			}
		}
	}()
}

// pushEnvelope sends one Socket Mode envelope to the connected client and
// waits for its ack to be recorded (or fails the test on timeout).
func (f *fakeSlack) pushEnvelope(t *testing.T, env envelope) {
	t.Helper()
	f.sockMu.Lock()
	conn := f.sockConn
	f.sockMu.Unlock()
	if conn == nil {
		t.Fatal("fake slack: no socket client connected yet")
	}
	if err := conn.WriteJSON(env); err != nil {
		t.Fatalf("fake slack: push envelope: %v", err)
	}
	f.waitAck(t, env.EnvelopeID)
}

// pushEvent wraps an inner event in an events_api envelope and pushes it.
func (f *fakeSlack) pushEvent(t *testing.T, ev slackEvent) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"type": "event_callback", "event": ev})
	f.pushEnvelope(t, envelope{EnvelopeID: "env-" + ev.TS + "-" + ev.Type, Type: "events_api", Payload: payload})
}

func (f *fakeSlack) waitAck(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, ack := range f.acks {
			if ack == id {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("fake slack: envelope %q was never acked", id)
}

// waitSocket blocks until a socket client is connected.
func (f *fakeSlack) waitSocket(t *testing.T) {
	t.Helper()
	select {
	case <-f.sockReady:
	case <-time.After(2 * time.Second):
		t.Fatal("fake slack: no socket client connected within 2s")
	}
}

// callsFor returns the recorded calls of one method.
func (f *fakeSlack) callsFor(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// callsSnapshot returns a copy of every recorded call, in order.
func (f *fakeSlack) callsSnapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// postCalls/updates shorthand the two assertions every routing test makes.
func (f *fakeSlack) postCalls() []fakeCall { return f.callsFor("chat.postMessage") }

// postsTo returns the chat.postMessage calls naming channel (any thread).
func (f *fakeSlack) postsTo(channel string) []fakeCall {
	var out []fakeCall
	for _, c := range f.postCalls() {
		if c.Body["channel"] == channel {
			out = append(out, c)
		}
	}
	return out
}

// uploads returns the completed file uploads.
func (f *fakeSlack) uploads() []fakeCall { return f.callsFor("files.completeUploadExternal") }

// ackedIDs returns every acked envelope id, in order.
func (f *fakeSlack) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acks...)
}

// totalCalls counts every Web API call (for "zero requests" assertions).
func (f *fakeSlack) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestFakeSlack_Self proves the fake records and rejects unknown methods: a
// typo in the client (posting to chat.postMessages) must fail the test, not
// pass silently as an unrecorded call. Drives the rejection through the
// production webAPI client so the shape it posts is the real one.
func TestFakeSlack_Self(t *testing.T) {
	f := newFakeSlack(t)
	f.quietUnknown = true // this run triggers the rejection on purpose
	api := newWebAPI(f.apiBase(), "xoxb-fake", f.httpc)
	if err := api.call(t.Context(), "chat.postMessages", map[string]any{}, nil); err == nil {
		t.Error("unknown method was accepted; the fake must reject it so client typos fail loudly")
	} else if !strings.Contains(err.Error(), "unknown_method_chat.postMessages") {
		t.Errorf("unknown method error = %v, want the loud unknown_method code", err)
	}
	if got := f.callsFor("chat.postMessages"); len(got) != 1 {
		t.Errorf("unknown method not recorded (%d calls)", len(got))
	}
}

// TestFakeSlack_UploadCompletionRequiresFilesArray proves the fake enforces
// files.completeUploadExternal's required wire shape like the real API: a
// completion without the "files" array (the file_id-only shape Slack
// rejects) fails with missing_argument, so a client-side regression cannot
// pass as a completed upload.
func TestFakeSlack_UploadCompletionRequiresFilesArray(t *testing.T) {
	f := newFakeSlack(t)
	api := newWebAPI(f.apiBase(), "xoxb-fake", f.httpc)
	if err := api.call(t.Context(), "files.completeUploadExternal", map[string]any{
		"file_id":  "F123",
		"channels": "C1",
	}, nil); err == nil {
		t.Error("completion without a files array was accepted; the real API rejects it with missing_argument")
	} else if !strings.Contains(err.Error(), "missing_argument") {
		t.Errorf("file_id-only completion error = %v, want missing_argument", err)
	}
	if err := api.call(t.Context(), "files.completeUploadExternal", map[string]any{
		"files":    []map[string]any{{"id": "F123", "title": "a.txt"}},
		"channels": "C1",
	}, nil); err != nil {
		t.Errorf("completion with a files array = %v, want accepted", err)
	}
}
