package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// recordingHandler returns an http.HandlerFunc that records the method,
// path, and body of each request. Tests inspect via mu-protected fields
// or via the helper getters below.
type recordingHandler struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

func (r *recordingHandler) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, recordedRequest{Method: req.Method, Path: req.URL.Path, Body: body})
		r.mu.Unlock()
		if req.URL.Path == "/session" && req.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"sess-inject"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (r *recordingHandler) countPath(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, req := range r.requests {
		if req.Path == path {
			n++
		}
	}
	return n
}

func (r *recordingHandler) lastPath(path string) (recordedRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Path == path {
			return r.requests[i], true
		}
	}
	return recordedRequest{}, false
}

// newReadyBackend returns a Backend whose Start has been called against
// the recordingHandler's httptest server. Caller can call Inject
// directly. Cleanup is registered via t.Cleanup.
func newReadyBackend(t *testing.T, rec *recordingHandler) *Backend {
	t.Helper()
	hs := httptest.NewServer(rec.handler())
	t.Cleanup(hs.Close)
	srv := &Server{
		baseURL:  hs.URL,
		http:     hs.Client(),
		agentID:  "test-inject",
		sessions: map[string]*Backend{},
	}
	b := &Backend{
		server:      srv,
		agentID:     "test-inject",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-inject"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// ---------------------------------------------------------------------------
// SourceUser at idle — the canonical begin-turn path
// ---------------------------------------------------------------------------

func TestInject_User_Idle_BeginsTurn(t *testing.T) {
	// Verifies Inject(SourceUser) at idle dispatches to the begin-turn
	// path: turnActive flips true, turnEvents is installed, and the
	// server receives POST /prompt_async with the prompt text in a
	// text part.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	turn := &delegator.TurnEvents{}
	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "hello world",
		Turn:   turn,
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after User-idle Inject")
	}
	b.turnMu.Lock()
	gotTurn := b.turnEvents
	b.turnMu.Unlock()
	if gotTurn != turn {
		t.Error("turnEvents was not installed")
	}

	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Errorf("POST /prompt_async fired %d times, want 1", got)
	}
	req, ok := rec.lastPath("/session/sess-inject/prompt_async")
	if !ok {
		t.Fatal("no /prompt_async request recorded")
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Parts) != 1 || body.Parts[0].Type != "text" || body.Parts[0].Text != "hello world" {
		t.Errorf("parts = %+v, want [{type:text text:%q}]", body.Parts, "hello world")
	}
}

func TestInject_User_Idle_WithAttachments(t *testing.T) {
	// Verifies attachments are converted to file parts with data: URLs
	// per plan §6.1. opencode treats them as first-class multimodal
	// content — same as a user pasting an image into the TUI.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	attachments := []delegator.Attachment{
		{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		{MimeType: "application/pdf", Data: []byte("%PDF-1.4")},
	}
	if err := b.Inject(context.Background(), delegator.Inject{
		Source:      delegator.SourceUser,
		Text:        "describe these",
		Attachments: attachments,
		Turn:        &delegator.TurnEvents{},
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	req, ok := rec.lastPath("/session/sess-inject/prompt_async")
	if !ok {
		t.Fatal("no /prompt_async request recorded")
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
			Mime string `json:"mime,omitempty"`
			URL  string `json:"url,omitempty"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3 (text + 2 attachments)", len(body.Parts))
	}
	if body.Parts[0].Type != "text" || body.Parts[0].Text != "describe these" {
		t.Errorf("parts[0] = %+v, want text/describe these", body.Parts[0])
	}
	for i, want := range []string{"image/png", "application/pdf"} {
		if body.Parts[1+i].Type != "file" {
			t.Errorf("parts[%d].type = %q, want file", 1+i, body.Parts[1+i].Type)
		}
		if body.Parts[1+i].Mime != want {
			t.Errorf("parts[%d].mime = %q, want %q", 1+i, body.Parts[1+i].Mime, want)
		}
		if !strings.HasPrefix(body.Parts[1+i].URL, "data:"+want+";base64,") {
			t.Errorf("parts[%d].url = %q, want data:%s;base64,<…>", 1+i, body.Parts[1+i].URL, want)
		}
	}
	// Verify the data: URL decodes back to the original bytes.
	for i, att := range attachments {
		url := body.Parts[1+i].URL
		prefix := "data:" + att.MimeType + ";base64,"
		encoded := strings.TrimPrefix(url, prefix)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("parts[%d] base64 decode: %v", 1+i, err)
			continue
		}
		if string(decoded) != string(att.Data) {
			t.Errorf("parts[%d] decoded = %v, want %v", 1+i, decoded, att.Data)
		}
	}
}

// ---------------------------------------------------------------------------
// SourceUser mid-turn — queued in steerBuf
// ---------------------------------------------------------------------------

func TestInject_User_InFlight_QueuedToSteerBuf(t *testing.T) {
	// Verifies Inject(SourceUser) during an in-flight turn does NOT
	// POST immediately — opencode has no mid-turn queue, so the text
	// is buffered in steerBuf for flushSteerBuf (called from Step 7's
	// OnSessionIdle). Replaces ccstream's fold-via-sendUser semantics
	// with the queue assertion the plan called out.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	// Manually flip into a "turn in flight" state without actually
	// sending a prompt (avoids needing Step 7's session.idle wiring).
	b.beginTurn(&delegator.TurnEvents{})

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "follow-up while busy",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// The follow-up must be in steerBuf, not POSTed.
	b.turnMu.Lock()
	buf := b.steerBuf
	b.turnMu.Unlock()
	if len(buf) != 1 || buf[0] != "follow-up while busy" {
		t.Errorf("steerBuf = %v, want [follow-up while busy]", buf)
	}
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 0 {
		t.Errorf("Inject mid-turn should NOT POST /prompt_async; got %d POSTs", got)
	}
}

// ---------------------------------------------------------------------------
// SourceSteer — queued mid-turn, degrades to User-idle when Turn present,
// returns ErrTurnNotInFlight when no Turn
// ---------------------------------------------------------------------------

func TestInject_Steer_InFlight_QueuedToSteerBuf_FlushedOnIdle(t *testing.T) {
	// Verifies the full steer lifecycle: steer arrives mid-turn → text
	// is buffered; flushSteerBuf is called → buffered text becomes a
	// follow-up turn via POST /prompt_async. This is the test plan §6.3
	// called out as the canonical opencode steer divergence from
	// ccstream.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	// Begin a turn (simulating a user turn already in progress).
	b.beginTurn(&delegator.TurnEvents{})

	// Two steers arrive mid-turn.
	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "first steer",
	}); err != nil {
		t.Fatalf("Inject first steer: %v", err)
	}
	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "second steer",
	}); err != nil {
		t.Fatalf("Inject second steer: %v", err)
	}

	// Nothing POSTed yet — both should be buffered.
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 0 {
		t.Errorf("pre-flush POSTs = %d, want 0", got)
	}

	// flushSteerBuf simulates Step 7's OnSessionIdle path.
	var newTurn delegator.TurnEvents
	if err := b.flushSteerBuf(context.Background(), func() *delegator.TurnEvents { return &newTurn }); err != nil {
		t.Fatalf("flushSteerBuf: %v", err)
	}

	// Exactly one POST should have fired — the two steers combined
	// into a single message with \n\n separator (plan §6.2 design
	// decision: combine to avoid multiplying model round-trips).
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Errorf("post-flush POSTs = %d, want 1 (combined follow-up turn)", got)
	}
	req, ok := rec.lastPath("/session/sess-inject/prompt_async")
	if !ok {
		t.Fatal("no /prompt_async recorded after flush")
	}
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Parts) != 1 || body.Parts[0].Text != "first steer\n\nsecond steer" {
		t.Errorf("combined text = %q, want %q", body.Parts[0].Text, "first steer\n\nsecond steer")
	}

	// steerBuf should be drained.
	b.turnMu.Lock()
	remaining := len(b.steerBuf)
	b.turnMu.Unlock()
	if remaining != 0 {
		t.Errorf("steerBuf len = %d after flush, want 0", remaining)
	}
}

func TestInject_Steer_Idle_BeginsTurn(t *testing.T) {
	// Verifies Inject(SourceSteer) at idle with inj.Turn present
	// degrades to User-idle (begin turn + sendPrompt). This handles
	// the race between turn end and platform queue dispatch — mirrors
	// ccstream's behaviour.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer at idle",
		Turn:   &delegator.TurnEvents{},
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after Steer-at-idle (should degrade to begin-turn)")
	}
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Errorf("Steer-at-idle POSTs = %d, want 1", got)
	}
}

func TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight(t *testing.T) {
	// Verifies Inject(SourceSteer) at idle with no inj.Turn returns
	// delegator.ErrTurnNotInFlight so the caller (Agent.Inbox) re-
	// routes through the normal idle path that builds a proper Turn.
	// Without this guard, beginning a turn here would lose
	// OnTurnComplete / usage accounting.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer at idle no turn",
	})
	if !errors.Is(err, delegator.ErrTurnNotInFlight) {
		t.Fatalf("Inject err = %v, want ErrTurnNotInFlight", err)
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true; a declined steer must not begin a turn")
	}
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 0 {
		t.Errorf("declined steer must not POST; got %d POSTs", got)
	}
}

// ---------------------------------------------------------------------------
// SourceCompact / SourcePass — slash commands
// ---------------------------------------------------------------------------

func TestInject_Compact(t *testing.T) {
	// Verifies Inject(SourceCompact) at idle POSTs /session/:id/summarize
	// with the last turn's provider+model. Compaction is NOT a /command:
	// opencode has no server-side "compact" command (Command.get returns
	// undefined → crash), so it uses the dedicated /summarize endpoint.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	// Simulate a completed assistant turn having captured the model.
	b.lastModel = "glm-5.2"
	b.lastProvider = "zai-coding-plan"

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact summarise everything",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := rec.countPath("/session/sess-inject/command"); got != 0 {
		t.Errorf("POST /command fired %d times, want 0 (compaction must not use /command)", got)
	}
	if got := rec.countPath("/session/sess-inject/summarize"); got != 1 {
		t.Errorf("POST /summarize fired %d times, want 1", got)
	}
	req, ok := rec.lastPath("/session/sess-inject/summarize")
	if !ok {
		t.Fatal("no /summarize request recorded")
	}
	var body struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
		Auto       bool   `json:"auto"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ProviderID != "zai-coding-plan" {
		t.Errorf("providerID = %q, want zai-coding-plan", body.ProviderID)
	}
	if body.ModelID != "glm-5.2" {
		t.Errorf("modelID = %q, want glm-5.2", body.ModelID)
	}
	if body.Auto {
		t.Errorf("auto = true, want false (foci triggers explicit compaction)")
	}
}

func TestInject_Compact_InFlight(t *testing.T) {
	// Verifies /compact is callable mid-turn without disturbing turn
	// state. beginTurn resets lastModel/lastProvider, so set them after.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.beginTurn(&delegator.TurnEvents{})
	b.lastModel = "glm-5.2"
	b.lastProvider = "zai-coding-plan"

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact x",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := rec.countPath("/session/sess-inject/summarize"); got != 1 {
		t.Errorf("POST /summarize fired %d times, want 1", got)
	}
}

func TestInject_Compact_NoModel(t *testing.T) {
	// With no captured model (no assistant turn yet), compaction fails
	// loudly rather than POSTing an empty model that opencode would reject.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact x",
	})
	if err == nil {
		t.Fatal("Inject: want error when no model captured, got nil")
	}
	if got := rec.countPath("/session/sess-inject/summarize"); got != 0 {
		t.Errorf("POST /summarize fired %d times, want 0 (no model → no request)", got)
	}
}

func TestInject_Pass(t *testing.T) {
	// Verifies SourcePass routes a passthrough slash command
	// (/context, /model, etc.) through /command.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.beginTurn(&delegator.TurnEvents{}) // mid-turn — should still fire

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourcePass,
		Text:   "/model opus",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	req, ok := rec.lastPath("/session/sess-inject/command")
	if !ok {
		t.Fatal("no /command request recorded")
	}
	var body struct {
		Command   string `json:"command"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Command != "model" {
		t.Errorf("command = %q, want model", body.Command)
	}
	if body.Arguments != "opus" {
		t.Errorf("arguments = %q, want opus", body.Arguments)
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestInject_BeforeStart(t *testing.T) {
	// Verifies Inject on a never-Started Backend returns an error
	// rather than panicking on a nil server or empty sessionID.
	b := &Backend{}
	err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "premature",
		Turn:   &delegator.TurnEvents{},
	})
	if err == nil {
		t.Error("Inject before Start should error")
	}
}

func TestInject_HTTPError(t *testing.T) {
	// Verifies sendPrompt surfaces a non-2xx response from
	// /prompt_async as an error rather than swallowing it.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"sess-err"}`))
			return
		}
		if r.URL.Path == "/session/sess-err/prompt_async" {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	defer hs.Close()
	srv := &Server{baseURL: hs.URL, http: hs.Client(), agentID: "test-err", sessions: map[string]*Backend{}}
	b := &Backend{server: srv, agentID: "test-err", readyCh: make(chan struct{}), outstanding: delegator.NewOutstandingRegistry()}
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-err"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "during rate limit",
		Turn:   &delegator.TurnEvents{},
	})
	if err == nil {
		t.Fatal("Inject should fail on HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want it to mention 429", err)
	}
}

// ---------------------------------------------------------------------------
// flushSteerBuf edge cases
// ---------------------------------------------------------------------------

func TestFlushSteerBuf_EmptyIsNoOp(t *testing.T) {
	// Verifies flushSteerBuf on an empty buffer is a no-op — no
	// sendPrompt, no beginTurn. Step 7's OnSessionIdle always calls
	// flushSteerBuf but most turns have nothing buffered; the no-op
	// path must be cheap and side-effect-free.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)

	turnCalled := false
	if err := b.flushSteerBuf(context.Background(), func() *delegator.TurnEvents {
		turnCalled = true
		return &delegator.TurnEvents{}
	}); err != nil {
		t.Fatalf("flushSteerBuf: %v", err)
	}
	if turnCalled {
		t.Error("turnFactory was called on empty steerBuf")
	}
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 0 {
		t.Errorf("flushSteerBuf on empty buffer POSTed %d times, want 0", got)
	}
}
