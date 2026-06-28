//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// newTestBackend returns a Backend wired with a buffered writer and the
// maps elicitation code touches. Tests use this instead of calling the real
// newFromConfig (which sets up channels we don't need here).
func newTestBackend(buf *bytes.Buffer) *Backend {
	return &Backend{
		writer:         NewWriter(nopWriteCloser{buf}),
		pendingPerms:   make(map[string]*pendingPermission),
		pendingElicits: make(map[string]*pendingElicitation),
		outstanding:    NewOutstandingRegistry(),
	}
}

// recordedPrompt captures the arguments passed to permPromptFn so tests can
// assert the text, summary, and button set sent to the platform.
type recordedPrompt struct {
	reqID   string
	text    string
	summary string
	choices []delegator.PromptChoice
}

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationURLMode proves a mode:"url" request stores state, calls
//// permPromptFn with Done/Decline/Cancel buttons, and emits a control_response
//// with action:"accept" and no content when the user clicks Done.
// func TestElicitationURLMode(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	var mu sync.Mutex
// 	var prompts []recordedPrompt
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		mu.Lock()
// 		defer mu.Unlock()
// 		prompts = append(prompts, recordedPrompt{reqID, text, summary, choices})
// 	}
//
// 	msg := &ElicitationRequest{
// 		RequestID: "elic-url",
// 		Request: ElicitationRequestPayload{
// 			Subtype:       "elicitation",
// 			McpServerName: "auth-server",
// 			Message:       "Please authorize in the browser.",
// 			Mode:          "url",
// 			URL:           "https://example.test/authorize?state=abc",
// 			ElicitationID: "e-42",
// 		},
// 	}
// 	b.OnElicitationRequest(msg)
//
// 	if b.PendingElicitations() != 1 {
// 		t.Fatalf("pending = %d, want 1", b.PendingElicitations())
// 	}
// 	if len(prompts) != 1 {
// 		t.Fatalf("permPromptFn called %d times, want 1", len(prompts))
// 	}
// 	p := prompts[0]
// 	if p.reqID != "elic-url" {
// 		t.Errorf("reqID = %q, want %q", p.reqID, "elic-url")
// 	}
// 	if !strings.Contains(p.text, "https://example.test/authorize") {
// 		t.Errorf("URL missing from prompt text: %q", p.text)
// 	}
// 	if !strings.Contains(p.text, "auth-server") {
// 		t.Errorf("server name missing from prompt text: %q", p.text)
// 	}
// 	labels := make([]string, 0, len(p.choices))
// 	for _, c := range p.choices {
// 		labels = append(labels, c.Label)
// 	}
// 	wantLabels := []string{"Done", "Decline", "Cancel"}
// 	if !equalStrSlice(labels, wantLabels) {
// 		t.Errorf("choices = %v, want %v", labels, wantLabels)
// 	}
//
// 	if err := b.RespondToElicitation("elic-url", "elic:accept"); err != nil {
// 		t.Fatalf("RespondToElicitation: %v", err)
// 	}
// 	if b.PendingElicitations() != 0 {
// 		t.Errorf("pending after accept = %d, want 0", b.PendingElicitations())
// 	}
// 	assertElicResponse(t, buf.String(), "accept", nil)
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationFormSingleField proves a one-field string schema walks to a
//// single prompt and sends {action:"accept", content:{"name":"Alice"}} after
//// the user replies with free text.
// func TestElicitationFormSingleField(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	var prompts []recordedPrompt
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		prompts = append(prompts, recordedPrompt{reqID, text, summary, choices})
// 	}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{"name":{"type":"string","description":"Your name"}},
// 		"required":["name"]
// 	}`)
// 	msg := &ElicitationRequest{
// 		RequestID: "elic-one",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			McpServerName:   "greeter",
// 			Message:         "Hello — who are you?",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	}
// 	b.OnElicitationRequest(msg)
//
// 	if len(prompts) != 1 {
// 		t.Fatalf("permPromptFn called %d times, want 1", len(prompts))
// 	}
// 	if !strings.Contains(prompts[0].text, "Your name") {
// 		t.Errorf("description missing from prompt text: %q", prompts[0].text)
// 	}
//
// 	if rid := b.HasPendingElicitation(); rid != "elic-one" {
// 		t.Errorf("HasPendingElicitation = %q, want %q", rid, "elic-one")
// 	}
//
// 	if err := b.RespondToElicitation("elic-one", "Alice"); err != nil {
// 		t.Fatalf("RespondToElicitation: %v", err)
// 	}
// 	if b.PendingElicitations() != 0 {
// 		t.Errorf("pending after last field = %d, want 0", b.PendingElicitations())
// 	}
//
// 	assertElicResponse(t, buf.String(), "accept", map[string]any{"name": "Alice"})
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationFormMultiField proves a two-field schema accumulates answers
//// across sequential prompts and only emits the control_response after the
//// final field is answered. Also proves the field order is preserved from
//// schema declaration order, not map iteration order.
// func TestElicitationFormMultiField(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	var prompts []recordedPrompt
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		prompts = append(prompts, recordedPrompt{reqID, text, summary, choices})
// 	}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{
// 			"name":{"type":"string"},
// 			"age":{"type":"integer"}
// 		},
// 		"required":["name","age"]
// 	}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-two",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			McpServerName:   "profile",
// 			Message:         "Tell us about yourself.",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	if len(prompts) != 1 {
// 		t.Fatalf("after first prompt: got %d prompts, want 1", len(prompts))
// 	}
// 	if buf.Len() != 0 {
// 		t.Errorf("wire output before all fields answered: %q", buf.String())
// 	}
//
// 	if err := b.RespondToElicitation("elic-two", "Alice"); err != nil {
// 		t.Fatalf("answer name: %v", err)
// 	}
// 	if len(prompts) != 2 {
// 		t.Fatalf("after second prompt: got %d prompts, want 2", len(prompts))
// 	}
// 	if buf.Len() != 0 {
// 		t.Errorf("wire output mid-walk: %q", buf.String())
// 	}
//
// 	if err := b.RespondToElicitation("elic-two", "30"); err != nil {
// 		t.Fatalf("answer age: %v", err)
// 	}
//
// 	assertElicResponse(t, buf.String(), "accept", map[string]any{
// 		"name": "Alice",
// 		"age":  float64(30), // JSON numbers decode as float64
// 	})
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationFormEnum proves enum properties render as buttons and the
//// chosen option's data maps back to the enum string when serialising content.
// func TestElicitationFormEnum(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	var prompts []recordedPrompt
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		prompts = append(prompts, recordedPrompt{reqID, text, summary, choices})
// 	}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{"color":{"type":"string","enum":["red","green","blue"]}},
// 		"required":["color"]
// 	}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-enum",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	if len(prompts) != 1 {
// 		t.Fatalf("prompts = %d, want 1", len(prompts))
// 	}
// 	choices := prompts[0].choices
// 	// Each enum value gets a button; Decline and Cancel are appended.
// 	wantDatas := []string{"elic:enum:0", "elic:enum:1", "elic:enum:2", "elic:decline", "elic:cancel"}
// 	gotDatas := make([]string, 0, len(choices))
// 	for _, c := range choices {
// 		gotDatas = append(gotDatas, c.Data)
// 	}
// 	if !equalStrSlice(gotDatas, wantDatas) {
// 		t.Errorf("choice data = %v, want %v", gotDatas, wantDatas)
// 	}
//
// 	// Free-text must NOT intercept when the active field is an enum.
// 	if rid := b.HasPendingElicitation(); rid != "" {
// 		t.Errorf("HasPendingElicitation on enum = %q, want empty", rid)
// 	}
//
// 	if err := b.RespondToElicitation("elic-enum", "elic:enum:1"); err != nil {
// 		t.Fatalf("RespondToElicitation: %v", err)
// 	}
// 	assertElicResponse(t, buf.String(), "accept", map[string]any{"color": "green"})
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationFormBoolean proves booleans render Yes/No buttons and map
//// to Go bools in the serialised content.
// func TestElicitationFormBoolean(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{"confirm":{"type":"boolean"}},
// 		"required":["confirm"]
// 	}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-bool",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	if err := b.RespondToElicitation("elic-bool", "elic:bool:true"); err != nil {
// 		t.Fatalf("RespondToElicitation: %v", err)
// 	}
// 	assertElicResponse(t, buf.String(), "accept", map[string]any{"confirm": true})
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationDecline proves the Decline button short-circuits the walk
//// and sends action:"decline" with no content, regardless of any answers
//// collected before the decline.
// func TestElicitationDecline(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{"a":{"type":"string"},"b":{"type":"string"}},
// 		"required":["a","b"]
// 	}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-dec",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	if err := b.RespondToElicitation("elic-dec", "first"); err != nil {
// 		t.Fatalf("first field: %v", err)
// 	}
// 	if err := b.RespondToElicitation("elic-dec", "elic:decline"); err != nil {
// 		t.Fatalf("decline: %v", err)
// 	}
// 	assertElicResponse(t, buf.String(), "decline", nil)
// 	if b.PendingElicitations() != 0 {
// 		t.Errorf("pending after decline = %d, want 0", b.PendingElicitations())
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationCancel proves Cancel sends action:"cancel" with no content.
// func TestElicitationCancel(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-can",
// 		Request: ElicitationRequestPayload{
// 			Subtype: "elicitation",
// 			Mode:    "url",
// 			URL:     "https://example.test/x",
// 		},
// 	})
// 	if err := b.RespondToElicitation("elic-can", "elic:cancel"); err != nil {
// 		t.Fatalf("RespondToElicitation: %v", err)
// 	}
// 	assertElicResponse(t, buf.String(), "cancel", nil)
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationURLCompletionNotification proves that an inbound
//// system/elicitation_complete whose id+server matches an in-flight URL-mode
//// elicitation auto-resolves it as accept, without the user clicking Done.
// func TestElicitationURLCompletionNotification(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-auto",
// 		Request: ElicitationRequestPayload{
// 			Subtype:       "elicitation",
// 			McpServerName: "remote",
// 			Mode:          "url",
// 			URL:           "https://example.test/login",
// 			ElicitationID: "srv-123",
// 		},
// 	})
// 	if b.PendingElicitations() != 1 {
// 		t.Fatalf("pending = %d, want 1", b.PendingElicitations())
// 	}
//
// 	b.OnElicitationComplete(&ElicitationCompleteMessage{
// 		Type:          "system",
// 		Subtype:       "elicitation_complete",
// 		McpServerName: "remote",
// 		ElicitationID: "srv-123",
// 	})
//
// 	if b.PendingElicitations() != 0 {
// 		t.Errorf("pending after completion = %d, want 0", b.PendingElicitations())
// 	}
// 	assertElicResponse(t, buf.String(), "accept", nil)
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationURLCompletionIgnoredForUnknownID proves completion
//// notifications for unknown ids are no-ops and don't disturb other
//// in-flight elicitations.
// func TestElicitationURLCompletionIgnoredForUnknownID(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-keep",
// 		Request: ElicitationRequestPayload{
// 			Subtype:       "elicitation",
// 			McpServerName: "remote",
// 			Mode:          "url",
// 			URL:           "https://example.test/x",
// 			ElicitationID: "known",
// 		},
// 	})
//
// 	b.OnElicitationComplete(&ElicitationCompleteMessage{
// 		Type:          "system",
// 		Subtype:       "elicitation_complete",
// 		McpServerName: "remote",
// 		ElicitationID: "stranger",
// 	})
//
// 	if b.PendingElicitations() != 1 {
// 		t.Errorf("pending after unrelated completion = %d, want 1", b.PendingElicitations())
// 	}
// 	if buf.Len() != 0 {
// 		t.Errorf("unexpected wire output: %q", buf.String())
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationUnparsableSchema proves that when the form schema is
//// missing or unsupported, the backend still presents a fallback prompt
//// offering only Decline and Cancel (never synthesising field values).
// func TestElicitationUnparsableSchema(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	var prompts []recordedPrompt
// 	b.permPromptFn = func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
// 		prompts = append(prompts, recordedPrompt{reqID, text, summary, choices})
// 	}
//
// 	// Arrays aren't in our supported subset.
// 	schema := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array"}}}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-fallback",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			Mode:            "form",
// 			Message:         "gimme an array",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	if len(prompts) != 1 {
// 		t.Fatalf("prompts = %d, want 1", len(prompts))
// 	}
// 	gotLabels := make([]string, 0, len(prompts[0].choices))
// 	for _, c := range prompts[0].choices {
// 		gotLabels = append(gotLabels, c.Label)
// 	}
// 	wantLabels := []string{"Decline", "Cancel"}
// 	if !equalStrSlice(gotLabels, wantLabels) {
// 		t.Errorf("fallback choices = %v, want %v", gotLabels, wantLabels)
// 	}
// 	// The fallback must never declare a pending free-text field.
// 	if rid := b.HasPendingElicitation(); rid != "" {
// 		t.Errorf("HasPendingElicitation on fallback = %q, want empty", rid)
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationFreeTextCoercionError proves that an invalid free-text
//// answer (e.g. "abc" for an integer field) returns an error and does not
//// advance the field cursor. The pending elicitation stays intact so the
//// platform can re-prompt the user.
// func TestElicitationFreeTextCoercionError(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	schema := json.RawMessage(`{
// 		"type":"object",
// 		"properties":{"n":{"type":"integer"}},
// 		"required":["n"]
// 	}`)
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-bad",
// 		Request: ElicitationRequestPayload{
// 			Subtype:         "elicitation",
// 			Mode:            "form",
// 			RequestedSchema: schema,
// 		},
// 	})
//
// 	err := b.RespondToElicitation("elic-bad", "not-a-number")
// 	if err == nil {
// 		t.Fatal("RespondToElicitation should error on bad integer input")
// 	}
// 	if b.PendingElicitations() != 1 {
// 		t.Errorf("pending after bad input = %d, want 1 (still awaiting valid answer)", b.PendingElicitations())
// 	}
// 	if buf.Len() != 0 {
// 		t.Errorf("wire output after rejected input: %q", buf.String())
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationUnknownRequestID proves RespondToElicitation errors cleanly
//// when called with a stale/unknown request ID.
// func TestElicitationUnknownRequestID(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	b := newTestBackend(&buf)
//
// 	if err := b.RespondToElicitation("nope", "elic:cancel"); err == nil {
// 		t.Fatal("expected error for unknown request ID")
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestReaderDispatchesElicitation proves the reader's control_request switch
//// correctly discriminates elicitation from can_use_tool and calls
//// OnElicitationRequest on the handler.
// func TestReaderDispatchesElicitation(t *testing.T) {
// 	t.Parallel()
//
// 	line := `{"type":"control_request","request_id":"rd-1","request":{"subtype":"elicitation","mcp_server_name":"srv","message":"hello","mode":"url","url":"https://x.test","elicitation_id":"eid"}}` + "\n"
//
// 	h := &mockHandler{}
// 	r := NewReader(strings.NewReader(line), h)
// 	r.Run(context.Background())
//
// 	if len(h.elicitations) != 1 {
// 		t.Fatalf("elicitations = %d, want 1", len(h.elicitations))
// 	}
// 	got := h.elicitations[0]
// 	if got.RequestID != "rd-1" {
// 		t.Errorf("request_id = %q, want %q", got.RequestID, "rd-1")
// 	}
// 	if got.Request.McpServerName != "srv" {
// 		t.Errorf("mcp_server_name = %q, want %q", got.Request.McpServerName, "srv")
// 	}
// 	if got.Request.Mode != "url" {
// 		t.Errorf("mode = %q, want %q", got.Request.Mode, "url")
// 	}
// 	if got.Request.URL != "https://x.test" {
// 		t.Errorf("url = %q, want %q", got.Request.URL, "https://x.test")
// 	}
// }

// TODO(opencode): rewrite — elicitation surfacing TBD — see plan section 10 for investigation
//// TestElicitationOnPromptsClearedFiresOnce proves that the registry's onEmpty
//// hook fires when the last in-flight user-interaction (permission OR
//// elicitation) resolves, not when each individual one does. Prevents the
//// platform's "has pending prompt" indicator from flapping.
// func TestElicitationOnPromptsClearedFiresOnce(t *testing.T) {
// 	t.Parallel()
//
// 	var buf bytes.Buffer
// 	var cleared int
// 	b := newTestBackend(&buf)
// 	b.SetOnPromptsCleared(func() { cleared++ })
// 	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {}
//
// 	// Two URL-mode elicitations outstanding.
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-a",
// 		Request:   ElicitationRequestPayload{Subtype: "elicitation", Mode: "url", URL: "x"},
// 	})
// 	b.OnElicitationRequest(&ElicitationRequest{
// 		RequestID: "elic-b",
// 		Request:   ElicitationRequestPayload{Subtype: "elicitation", Mode: "url", URL: "y"},
// 	})
//
// 	if err := b.RespondToElicitation("elic-a", "elic:cancel"); err != nil {
// 		t.Fatalf("cancel a: %v", err)
// 	}
// 	if cleared != 0 {
// 		t.Errorf("cleared after first resolve = %d, want 0 (still one pending)", cleared)
// 	}
// 	if err := b.RespondToElicitation("elic-b", "elic:cancel"); err != nil {
// 		t.Fatalf("cancel b: %v", err)
// 	}
// 	if cleared != 1 {
// 		t.Errorf("cleared after last resolve = %d, want 1", cleared)
// 	}
// }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// assertElicResponse verifies that exactly one control_response was written
// with the expected action and (optionally) content map.
func assertElicResponse(t *testing.T, raw, wantAction string, wantContent map[string]any) {
	t.Helper()
	line := strings.TrimSpace(raw)
	if line == "" {
		t.Fatal("no wire output (expected control_response)")
	}
	if i := strings.IndexByte(line, '\n'); i != -1 {
		t.Fatalf("multiple wire messages, want 1: %q", line)
	}

	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string                     `json:"subtype"`
			RequestID string                     `json:"request_id"`
			Response  ElicitationResponsePayload `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, line)
	}
	if envelope.Type != "control_response" {
		t.Errorf("type = %q, want %q", envelope.Type, "control_response")
	}
	if envelope.Response.Subtype != "success" {
		t.Errorf("subtype = %q, want %q", envelope.Response.Subtype, "success")
	}
	if envelope.Response.Response.Action != wantAction {
		t.Errorf("action = %q, want %q", envelope.Response.Response.Action, wantAction)
	}

	if wantContent == nil {
		if len(envelope.Response.Response.Content) != 0 {
			t.Errorf("content = %s, want none", envelope.Response.Response.Content)
		}
		return
	}
	var gotContent map[string]any
	if err := json.Unmarshal(envelope.Response.Response.Content, &gotContent); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if len(gotContent) != len(wantContent) {
		t.Errorf("content keys = %d, want %d (%v vs %v)", len(gotContent), len(wantContent), gotContent, wantContent)
	}
	for k, want := range wantContent {
		got, ok := gotContent[k]
		if !ok {
			t.Errorf("content[%q] missing", k)
			continue
		}
		if got != want {
			t.Errorf("content[%q] = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
