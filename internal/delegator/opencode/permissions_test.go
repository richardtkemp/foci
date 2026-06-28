package opencode

import (
	"encoding/json"
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

// newPermTestBackend returns a Backend wired to an httptest server that
// records POST /permissions/:id requests, plus a capturing permPromptFn.
func newPermTestBackend(t *testing.T) (*Backend, *permRecorder) {
	t.Helper()
	rec := &permRecorder{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, permRequest{Path: r.URL.Path, Body: body})
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)

	var promptMu sync.Mutex
	var lastPrompt struct {
		id      string
		text    string
		choices []delegator.PromptChoice
	}

	b := &Backend{
		server:      &Server{baseURL: hs.URL, http: hs.Client(), agentID: "perm-test"},
		agentID:     "perm-test",
		sessionID:   "sess-perm",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.permPromptFn = func(id, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		promptMu.Lock()
		lastPrompt.id = id
		lastPrompt.text = text
		lastPrompt.choices = choices
		promptMu.Unlock()
	}

	rec.backend = b
	rec.promptMu = &promptMu
	rec.lastPrompt = &lastPrompt
	return b, rec
}

type permRequest struct {
	Path string
	Body []byte
}

type permRecorder struct {
	mu         sync.Mutex
	requests   []permRequest
	backend    *Backend
	promptMu   *sync.Mutex
	lastPrompt *struct {
		id      string
		text    string
		choices []delegator.PromptChoice
	}
}

func (r *permRecorder) lastPermPost() (permRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if len(r.requests[i].Path) > 12 && r.requests[i].Path[len(r.requests[i].Path)-11:] == "/permissions" {
			return r.requests[i], true
		}
	}
	// Also check /permissions/:id pattern
	for i := len(r.requests) - 1; i >= 0; i-- {
		if strings.Contains(r.requests[i].Path, "/permissions/") {
			return r.requests[i], true
		}
	}
	return permRequest{}, false
}

func (r *permRecorder) promptInfo() (string, string, []delegator.PromptChoice) {
	r.promptMu.Lock()
	defer r.promptMu.Unlock()
	return r.lastPrompt.id, r.lastPrompt.text, r.lastPrompt.choices
}

// ---------------------------------------------------------------------------
// onPermissionUpdated — regular permissions
// ---------------------------------------------------------------------------

func TestOnPermissionUpdated_StoresAndPrompts(t *testing.T) {
	// Verifies onPermissionUpdated stores the permission and surfaces
	// it via permPromptFn with Allow/Deny/Always-Allow choices.
	b, rec := newPermTestBackend(t)

	b.onPermissionUpdated(Permission{
		ID:        "perm-1",
		Type:      PermBash,
		Title:     "Run bash: git status",
		SessionID: "sess-perm",
		MessageID: "msg-1",
		Metadata:  json.RawMessage(`{}`),
	})

	// Verify stored.
	b.permMu.Lock()
	pp, ok := b.pendingPerms["perm-1"]
	b.permMu.Unlock()
	if !ok {
		t.Fatal("permission not stored in pendingPerms")
	}
	if pp.permType != PermBash {
		t.Errorf("permType = %q, want %q", pp.permType, PermBash)
	}

	// Verify prompted.
	id, _, choices := rec.promptInfo()
	if id != "perm-1" {
		t.Errorf("prompted id = %q, want perm-1", id)
	}
	if len(choices) != 3 {
		t.Fatalf("choices = %d, want 3 (Allow/Deny/Always)", len(choices))
	}
	want := []string{"Allow", "Deny", "Always Allow"}
	for i, w := range want {
		if choices[i].Label != w {
			t.Errorf("choices[%d].Label = %q, want %q", i, choices[i].Label, w)
		}
	}
}

func TestOnPermissionUpdated_QuestionRoutesToQuestionPath(t *testing.T) {
	// Verifies a question-type permission renders with option-based
	// choices (from metadata) rather than the binary Allow/Deny.
	b, rec := newPermTestBackend(t)

	meta, _ := json.Marshal(questionMetadata{
		Header: "Flavour",
		Text:   "Which flavour?",
		Options: []questionOption{
			{Label: "Vanilla"},
			{Label: "Chocolate"},
		},
	})
	b.onPermissionUpdated(Permission{
		ID:        "perm-q1",
		Type:      PermQuestion,
		Title:     "Pick a flavour",
		SessionID: "sess-perm",
		MessageID: "msg-1",
		Metadata:  meta,
	})

	id, text, choices := rec.promptInfo()
	if id != "perm-q1" {
		t.Errorf("prompted id = %q, want perm-q1", id)
	}
	if text != "Flavour: Which flavour?" {
		t.Errorf("prompt text = %q, want 'Flavour: Which flavour?'", text)
	}
	if len(choices) != 2 {
		t.Fatalf("choices = %d, want 2 (Vanilla/Chocolate)", len(choices))
	}
	if choices[0].Label != "Vanilla" || choices[1].Label != "Chocolate" {
		t.Errorf("choices = %v, want [Vanilla Chocolate]", choices)
	}
}

func TestOnPermissionUpdated_QuestionStoresAndPrompts(t *testing.T) {
	// Verifies a question-type permission is STORED in pendingPerms
	// (with the right type) AND surfaced via permPromptFn. Separate
	// from QuestionRoutesToQuestionPath which asserts the option-based
	// rendering; this test pins the storage + prompting contract that
	// QuestionResponder relies on.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{
		Header: "Test", Text: "Pick one",
		Options: []questionOption{{Label: "A"}, {Label: "B"}},
	})

	b.onPermissionUpdated(Permission{
		ID:        "perm-q-store",
		Type:      PermQuestion,
		Title:     "Pick one",
		SessionID: "sess-perm",
		MessageID: "msg-1",
		Metadata:  meta,
	})

	// Stored with the right type.
	b.permMu.Lock()
	pp, ok := b.pendingPerms["perm-q-store"]
	b.permMu.Unlock()
	if !ok {
		t.Fatal("question permission not stored in pendingPerms")
	}
	if pp.permType != PermQuestion {
		t.Errorf("permType = %q, want %q", pp.permType, PermQuestion)
	}

	// Prompted.
	id, _, _ := rec.promptInfo()
	if id != "perm-q-store" {
		t.Errorf("permPromptFn called with id = %q, want perm-q-store", id)
	}

	// Registered in outstanding.
	if !b.outstanding.Has("perm-q-store") {
		t.Error("question not registered in OutstandingRegistry")
	}
}

func TestOnPermissionUpdated_NilPermPromptFn(t *testing.T) {
	// Verifies nil permPromptFn doesn't panic — just logs a warning.
	b := &Backend{
		sessionID:    "sess-test",
		outstanding:  delegator.NewOutstandingRegistry(),
		pendingPerms: make(map[string]*pendingPermission),
	}
	b.onPermissionUpdated(Permission{
		ID:    "perm-nil",
		Type:  PermBash,
		Title: "test",
	})
	// Should be stored even if not displayed.
	b.permMu.Lock()
	_, ok := b.pendingPerms["perm-nil"]
	b.permMu.Unlock()
	if !ok {
		t.Error("permission not stored when permPromptFn is nil")
	}
}

// ---------------------------------------------------------------------------
// RespondToPermission
// ---------------------------------------------------------------------------

func TestRespondToPermission_AllowPostsAndResolves(t *testing.T) {
	b, rec := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-a", Type: PermBash, Title: "test",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	if err := b.RespondToPermission("perm-a", true, false); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	post, ok := rec.lastPermPost()
	if !ok {
		t.Fatal("no POST /permissions recorded")
	}
	var body struct {
		Response string `json:"response"`
	}
	json.Unmarshal(post.Body, &body)
	if body.Response != "allow" {
		t.Errorf("response = %q, want allow", body.Response)
	}

	// Should be resolved from outstanding + removed from pendingPerms.
	if b.outstanding.Has("perm-a") {
		t.Error("permission still in outstanding after RespondToPermission")
	}
}

func TestRespondToPermission_DenyPostsAndResolves(t *testing.T) {
	b, rec := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-d", Type: PermBash, Title: "test",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	if err := b.RespondToPermission("perm-d", false, false); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	post, _ := rec.lastPermPost()
	var body struct {
		Response string `json:"response"`
	}
	json.Unmarshal(post.Body, &body)
	if body.Response != "deny" {
		t.Errorf("response = %q, want deny", body.Response)
	}
}

func TestRespondToPermission_AlwaysAllowPassesRememberTrue(t *testing.T) {
	b, rec := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-aa", Type: PermBash, Title: "test",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	if err := b.RespondToPermission("perm-aa", true, true); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	post, _ := rec.lastPermPost()
	var body struct {
		Remember bool `json:"remember"`
	}
	json.Unmarshal(post.Body, &body)
	if !body.Remember {
		t.Error("remember = false, want true for Always Allow")
	}
}

func TestRespondToPermission_UnknownIDReturnsError(t *testing.T) {
	b, _ := newPermTestBackend(t)
	err := b.RespondToPermission("nonexistent", true, false)
	if err == nil {
		t.Error("expected error for unknown permission ID")
	}
}

// ---------------------------------------------------------------------------
// onPermissionReplied — out-of-band cancel
// ---------------------------------------------------------------------------

func TestOnPermissionReplied_FiresCancelListenersAndResolves(t *testing.T) {
	// Verifies onPermissionReplied fires cancel listeners (so the
	// platform disables the orphaned keyboard) and resolves from the
	// outstanding registry.
	b, _ := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-reply", Type: PermBash, Title: "test",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	// Register a cancel listener.
	var cancelFired bool
	var cancelReason string
	b.outstanding.AddCancelListener("perm-reply", func(reason string) {
		cancelFired = true
		cancelReason = reason
	})

	b.onPermissionReplied("sess-perm", "perm-reply", "allow")

	if !cancelFired {
		t.Error("cancel listener was not fired")
	}
	if cancelReason != "replied out-of-band" {
		t.Errorf("cancel reason = %q, want 'replied out-of-band'", cancelReason)
	}
	if b.outstanding.Has("perm-reply") {
		t.Error("permission still in outstanding after onPermissionReplied")
	}
	b.permMu.Lock()
	_, ok := b.pendingPerms["perm-reply"]
	b.permMu.Unlock()
	if ok {
		t.Error("permission still in pendingPerms after onPermissionReplied")
	}
}

// ---------------------------------------------------------------------------
// QuestionResponder
// ---------------------------------------------------------------------------

func TestRespondToQuestion_OptionClickPostsResponse(t *testing.T) {
	// Verifies RespondToQuestion with an option label POSTs it as the
	// response body's `response` field.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{
		Header: "Test", Text: "Pick one",
		Options: []questionOption{{Label: "Option A"}, {Label: "Option B"}},
	})
	b.onPermissionUpdated(Permission{
		ID: "perm-q-click", Type: PermQuestion, Title: "Pick one",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	if err := b.RespondToQuestion("perm-q-click", "Option A"); err != nil {
		t.Fatalf("RespondToQuestion: %v", err)
	}

	post, _ := rec.lastPermPost()
	var body struct {
		Response string `json:"response"`
	}
	json.Unmarshal(post.Body, &body)
	if body.Response != "Option A" {
		t.Errorf("response = %q, want Option A", body.Response)
	}
}

func TestRespondToQuestion_TypedAnswerPostsResponse(t *testing.T) {
	// Verifies a typed custom answer is sent as the response.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{
		Header: "Test", Text: "Enter name",
	})
	b.onPermissionUpdated(Permission{
		ID: "perm-q-type", Type: PermQuestion, Title: "Enter name",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	if err := b.RespondToQuestion("perm-q-type", "my custom answer"); err != nil {
		t.Fatalf("RespondToQuestion: %v", err)
	}

	post, _ := rec.lastPermPost()
	var body struct {
		Response string `json:"response"`
	}
	json.Unmarshal(post.Body, &body)
	if body.Response != "my custom answer" {
		t.Errorf("response = %q, want 'my custom answer'", body.Response)
	}
}

func TestCancelQuestion_PostsDecline(t *testing.T) {
	// Verifies CancelQuestion sends "deny" as the response.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{Header: "Test", Text: "?"})
	b.onPermissionUpdated(Permission{
		ID: "perm-q-cancel", Type: PermQuestion, Title: "?",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	if err := b.CancelQuestion("perm-q-cancel"); err != nil {
		t.Fatalf("CancelQuestion: %v", err)
	}

	post, _ := rec.lastPermPost()
	var body struct {
		Response string `json:"response"`
	}
	json.Unmarshal(post.Body, &body)
	if body.Response != "deny" {
		t.Errorf("response = %q, want deny", body.Response)
	}
}

func TestHasPendingQuestion_ReturnsIDOrEmpty(t *testing.T) {
	// Verifies HasPendingQuestion returns the ID of a pending question.
	b, _ := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{Header: "Test", Text: "?"})

	if id := b.HasPendingQuestion(); id != "" {
		t.Errorf("HasPendingQuestion = %q before any question, want empty", id)
	}

	b.onPermissionUpdated(Permission{
		ID: "perm-q-has", Type: PermQuestion, Title: "?",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	if id := b.HasPendingQuestion(); id != "perm-q-has" {
		t.Errorf("HasPendingQuestion = %q, want perm-q-has", id)
	}
}

func TestHasPendingQuestion_DoesNotReturnPermissionIDs(t *testing.T) {
	// Verifies HasPendingQuestion doesn't return a non-question
	// permission ID. Guards against confusing a regular permission
	// with a question.
	b, _ := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-bash", Type: PermBash, Title: "git status",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	if id := b.HasPendingQuestion(); id != "" {
		t.Errorf("HasPendingQuestion = %q for a bash permission, want empty", id)
	}
}

func TestRespondToQuestion_OnNonQuestionReturnsError(t *testing.T) {
	// Verifies calling RespondToQuestion on a non-question permission
	// returns an error rather than sending a wrong-type response.
	b, _ := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-bash-rq", Type: PermBash, Title: "test",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	err := b.RespondToQuestion("perm-bash-rq", "allow")
	if err == nil {
		t.Error("RespondToQuestion on a non-question should error")
	}
}
