package codex

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// methodSequence records the order of JSON-RPC methods received by the mock
// app-server. It is safe for concurrent use: the mock reader goroutine records
// while the test goroutine reads a snapshot after the request round-trips.
type methodSequence struct {
	mu      sync.Mutex
	methods []string
}

func (s *methodSequence) record(m string) {
	s.mu.Lock()
	s.methods = append(s.methods, m)
	s.mu.Unlock()
}

func (s *methodSequence) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.methods))
	copy(out, s.methods)
	return out
}

// newStartableBackend returns a Backend from setupMockBackend with the
// readyCh/readyOnce pre-armed so startThread/resumeThread can fire their
// ready signal without panicking on a nil channel.
func newStartableBackend(t *testing.T, handler func(method string, params json.RawMessage, id int64) (json.RawMessage, error)) *Backend {
	t.Helper()
	b := setupMockBackend(t, handler)
	b.readyCh = make(chan struct{})
	return b
}

// --- startThread ---

// TestStartThread_PassesBaseInstructions verifies the mock app-server receives
// thread/start with baseInstructions matching StartOptions.SystemPrompt.
func TestStartThread_PassesBaseInstructions(t *testing.T) {
	const wantPrompt = "you are a careful coding agent"

	var gotBaseInstructions string
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		if method != "thread/start" {
			t.Errorf("unexpected method %q, want thread/start", method)
			return json.RawMessage(`{}`), nil
		}
		var p threadStartParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("unmarshal thread/start params: %v", err)
		}
		gotBaseInstructions = p.BaseInstructions
		return json.RawMessage(`{"thread":{"id":"th_1"}}`), nil
	})
	b.workDir = "/tmp"
	b.startOpts = delegator.StartOptions{SystemPrompt: wantPrompt}

	if _, err := b.startThread(); err != nil {
		t.Fatalf("startThread: %v", err)
	}
	if gotBaseInstructions != wantPrompt {
		t.Errorf("baseInstructions = %q, want %q", gotBaseInstructions, wantPrompt)
	}
}

// TestStartThread_PassesSandbox verifies the mock app-server receives
// thread/start with sandbox matching the configured sandbox mode.
func TestStartThread_PassesSandbox(t *testing.T) {
	const wantSandbox = "read-only"

	var gotSandbox string
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		var p threadStartParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("unmarshal thread/start params: %v", err)
		}
		gotSandbox = p.Sandbox
		return json.RawMessage(`{"thread":{"id":"th_2"}}`), nil
	})
	b.cfg = map[string]any{"sandbox": wantSandbox}
	b.workDir = "/tmp"

	if _, err := b.startThread(); err != nil {
		t.Fatalf("startThread: %v", err)
	}
	if gotSandbox != wantSandbox {
		t.Errorf("sandbox = %q, want %q", gotSandbox, wantSandbox)
	}
}

// TestStartThread_CapturesModel verifies the model returned in thread/start is
// stored on the backend and surfaces as "codex/<model>" in TurnResult.Model
// after a turn/completed notification.
func TestStartThread_CapturesModel(t *testing.T) {
	const serverModel = "gpt-5.6-luna"

	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		return json.RawMessage(`{"thread":{"id":"th_3"},"model":"` + serverModel + `"}`), nil
	})
	b.workDir = "/tmp"

	if _, err := b.startThread(); err != nil {
		t.Fatalf("startThread: %v", err)
	}

	// startThread must have stashed the raw model (un-prefixed).
	b.mu.Lock()
	stashedModel := b.model
	b.mu.Unlock()
	if stashedModel != serverModel {
		t.Fatalf("b.model = %q, want %q", stashedModel, serverModel)
	}

	// Drive a turn/completed through dispatch and capture the TurnResult; the
	// model must be prefixed with "codex/" on the way out (see onTurnCompleted).
	var got *delegator.TurnResult
	openTurn(b, &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	})
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"th_3","turn":{"id":"tu_1","status":"completed"}}}`))

	if got == nil {
		t.Fatal("OnTurnComplete was not fired for turn/completed")
	}
	if want := "codex/" + serverModel; got.Model != want {
		t.Errorf("TurnResult.Model = %q, want %q", got.Model, want)
	}
}

// --- triggerCompaction ---

// TestTriggerCompaction_WritesCompactPromptBeforeCompaction verifies that when
// CompactionPromptFunc returns a prompt, config/value/write (key
// "compact_prompt") is sent BEFORE thread/compact/start. Ordering is asserted
// by capturing the sequence of methods received by the mock app-server.
func TestTriggerCompaction_WritesCompactPromptBeforeCompaction(t *testing.T) {
	const wantPrompt = "summarise the session focusing on tests"
	var seq methodSequence
	var gotCompactKey, gotCompactValue string

	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		seq.record(method)
		if method == "config/value/write" {
			var p struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("unmarshal config/value/write params: %v", err)
			}
			gotCompactKey = p.Key
			gotCompactValue = p.Value
		}
		return json.RawMessage(`{}`), nil
	})
	b.threadID = "th_compact"
	b.startOpts = delegator.StartOptions{
		CompactionPromptFunc: func(sessionKey string) string { return wantPrompt },
	}

	if err := b.triggerCompaction(); err != nil {
		t.Fatalf("triggerCompaction: %v", err)
	}

	methods := seq.snapshot()
	wantSequence := []string{"config/value/write", "thread/compact/start"}
	if len(methods) != len(wantSequence) {
		t.Fatalf("method sequence = %v, want %v", methods, wantSequence)
	}
	for i, m := range methods {
		if m != wantSequence[i] {
			t.Errorf("methods[%d] = %q, want %q (full sequence %v)", i, m, wantSequence[i], methods)
		}
	}
	if gotCompactKey != "compact_prompt" {
		t.Errorf("config/value/write key = %q, want %q", gotCompactKey, "compact_prompt")
	}
	if gotCompactValue != wantPrompt {
		t.Errorf("config/value/write value = %q, want %q", gotCompactValue, wantPrompt)
	}
}

// TestTriggerCompaction_NoCompactionPromptFunc verifies that without a
// CompactionPromptFunc the backend goes straight to thread/compact/start with
// no preceding config/value/write.
func TestTriggerCompaction_NoCompactionPromptFunc(t *testing.T) {
	var seq methodSequence

	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		seq.record(method)
		return json.RawMessage(`{}`), nil
	})
	b.threadID = "th_compact2"
	// CompactionPromptFunc intentionally left nil.

	if err := b.triggerCompaction(); err != nil {
		t.Fatalf("triggerCompaction: %v", err)
	}

	methods := seq.snapshot()
	for _, m := range methods {
		if m == "config/value/write" {
			t.Errorf("config/value/write must not be sent without CompactionPromptFunc; sequence %v", methods)
		}
	}
	if len(methods) != 1 || methods[0] != "thread/compact/start" {
		t.Errorf("method sequence = %v, want [thread/compact/start]", methods)
	}
}

// TestTriggerCompaction_NoThreadErrors verifies triggerCompaction returns an
// error (and contacts no server) when there is no active thread.
func TestTriggerCompaction_NoThreadErrors(t *testing.T) {
	handlerCalled := false
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		handlerCalled = true
		return json.RawMessage(`{}`), nil
	})
	// b.threadID left empty.

	err := b.triggerCompaction()
	if err == nil {
		t.Fatal("expected error when no active thread, got nil")
	}
	if !strings.Contains(err.Error(), "no active thread") {
		t.Errorf("error = %v, want it to contain %q", err, "no active thread")
	}
	if handlerCalled {
		t.Error("app-server handler must not be called when there is no active thread")
	}
}

// --- initialize ---

// TestInitialize_SendsClientInfoVersion verifies the initialize request
// includes clientInfo.version populated from cfg["foci_version"].
func TestInitialize_SendsClientInfoVersion(t *testing.T) {
	const wantVersion = "9.9.9"

	var gotVersion string
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		if method != "initialize" {
			t.Errorf("unexpected method %q, want initialize", method)
			return json.RawMessage(`{}`), nil
		}
		var p initializeParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("unmarshal initialize params: %v", err)
		}
		gotVersion = p.ClientInfo.Version
		return json.RawMessage(`{}`), nil
	})
	b.cfg = map[string]any{"foci_version": wantVersion}

	if err := b.initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if gotVersion != wantVersion {
		t.Errorf("clientInfo.version = %q, want %q", gotVersion, wantVersion)
	}
}

// --- resumeThread ---

// TestResumeThread_SendsThreadResume verifies resumeThread sends a
// thread/resume request carrying the target thread id.
func TestResumeThread_SendsThreadResume(t *testing.T) {
	const wantThreadID = "thread-resume-abc"

	var gotMethod, gotThreadID string
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		gotMethod = method
		var p threadResumeParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("unmarshal thread/resume params: %v", err)
		}
		gotThreadID = p.ThreadID
		return json.RawMessage(`{}`), nil
	})

	if err := b.resumeThread(wantThreadID); err != nil {
		t.Fatalf("resumeThread: %v", err)
	}
	if gotMethod != "thread/resume" {
		t.Errorf("sent method %q, want thread/resume", gotMethod)
	}
	if gotThreadID != wantThreadID {
		t.Errorf("sent threadId %q, want %q", gotThreadID, wantThreadID)
	}

	// resumeThread must also persist the thread id on the backend.
	if got := b.SessionID(); got != wantThreadID {
		t.Errorf("SessionID() = %q, want %q", got, wantThreadID)
	}
}
