package turn

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"foci/internal/log"
)

// mockTrackerBackend records all calls for test assertions.
type mockTrackerBackend struct {
	mu sync.Mutex

	sends           []string
	sendWithButtons []mockTrackerSendBtn
	edits           []mockTrackerEdit
	editWithButtons []mockTrackerEditBtn
	deletes         []string

	nextMsgID int
	sendErr   error
	editErr   error
}

type mockTrackerSendBtn struct {
	text, btnLabel, btnData string
}

type mockTrackerEdit struct {
	msgID, text string
}

type mockTrackerEditBtn struct {
	msgID, text, btnLabel, btnData string
}

func (m *mockTrackerBackend) FormatCompact(toolName string, params json.RawMessage) string {
	return fmt.Sprintf("[compact] %s", toolName)
}

func (m *mockTrackerBackend) FormatFull(toolName string, params json.RawMessage, showMode string) string {
	return fmt.Sprintf("[full:%s] %s %s", showMode, toolName, string(params))
}

func (m *mockTrackerBackend) FormatWithResult(toolText, result string) string {
	return toolText + "\n---\n" + result
}

func (m *mockTrackerBackend) FormatHintSuffix(hint string) string {
	return " -> " + hint
}

func (m *mockTrackerBackend) FormatRetry(endpoint string) string {
	return fmt.Sprintf("retrying %s...", endpoint)
}

func (m *mockTrackerBackend) FormatRetryClear() string {
	return "request completed"
}

func (m *mockTrackerBackend) Send(text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, text)
	if m.sendErr != nil {
		return "", m.sendErr
	}
	m.nextMsgID++
	return fmt.Sprintf("msg-%d", m.nextMsgID), nil
}

func (m *mockTrackerBackend) SendWithButton(text, btnLabel, btnData string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendWithButtons = append(m.sendWithButtons, mockTrackerSendBtn{text, btnLabel, btnData})
	if m.sendErr != nil {
		return "", m.sendErr
	}
	m.nextMsgID++
	return fmt.Sprintf("msg-%d", m.nextMsgID), nil
}

func (m *mockTrackerBackend) Edit(msgID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits = append(m.edits, mockTrackerEdit{msgID, text})
	return m.editErr
}

func (m *mockTrackerBackend) EditWithButton(msgID, text, btnLabel, btnData string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.editWithButtons = append(m.editWithButtons, mockTrackerEditBtn{msgID, text, btnLabel, btnData})
	return m.editErr
}

func (m *mockTrackerBackend) Delete(msgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, msgID)
	return nil
}

func (m *mockTrackerBackend) Logger() *log.ComponentLogger {
	return log.NewComponentLogger("test")
}

func (m *mockTrackerBackend) sendCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sends)
}

func (m *mockTrackerBackend) sendWithButtonCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sendWithButtons)
}

func (m *mockTrackerBackend) editCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.edits)
}

func (m *mockTrackerBackend) editWithButtonCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.editWithButtons)
}

func (m *mockTrackerBackend) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deletes)
}

// mockTrackerStore records store operations for test assertions.
type mockTrackerStore struct {
	mu      sync.Mutex
	entries map[string]mockTrackerEntry
	persisted []mockTrackerPersisted
}

type mockTrackerEntry struct {
	compact, full, result string
	expanded              bool
}

type mockTrackerPersisted struct {
	msgID, compact, full, result string
}

func newMockTrackerStore() *mockTrackerStore {
	return &mockTrackerStore{entries: make(map[string]mockTrackerEntry)}
}

func (s *mockTrackerStore) StoreEntry(msgID, compact, full, result string, expanded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[msgID] = mockTrackerEntry{compact, full, result, expanded}
}

func (s *mockTrackerStore) IsExpanded(msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[msgID].expanded
}

func (s *mockTrackerStore) Persist(msgID, compact, full, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persisted = append(s.persisted, mockTrackerPersisted{msgID, compact, full, result})
}

func noHint(string, json.RawMessage, string) string { return "" }

func TestObserveToolCall_OffMode(t *testing.T) {
	// Verifies that tool calls are completely suppressed when mode is "off".
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "off"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))

	if backend.sendCount()+backend.sendWithButtonCount() != 0 {
		t.Error("off mode should not send any messages")
	}
}

func TestObserveToolCall_EmptyMode(t *testing.T) {
	// Verifies that empty mode (default) is treated the same as "off".
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: ""}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))

	if backend.sendCount()+backend.sendWithButtonCount() != 0 {
		t.Error("empty mode should not send any messages")
	}
}

func TestObserveToolCall_FullMode(t *testing.T) {
	// Verifies that full mode sends each tool call as a new message with a button
	// and stores the entry for later expansion.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))

	if backend.sendWithButtonCount() != 1 {
		t.Fatalf("sendWithButton count = %d, want 1", backend.sendWithButtonCount())
	}
	sb := backend.sendWithButtons[0]
	if sb.btnLabel != "Show full" || sb.btnData != "tc:show" {
		t.Errorf("button = (%q, %q), want (Show full, tc:show)", sb.btnLabel, sb.btnData)
	}
	if tracker.LastMsgID() != "msg-1" {
		t.Errorf("msgID = %q, want msg-1", tracker.LastMsgID())
	}

	// Second tool call in full mode also sends a new message.
	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if backend.sendWithButtonCount() != 2 {
		t.Errorf("sendWithButton count = %d, want 2", backend.sendWithButtonCount())
	}
}

func TestObserveToolCall_PreviewMode(t *testing.T) {
	// Verifies that preview mode sends on first call and edits on subsequent
	// calls (overwriting the same message).
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "preview"}, noHint)

	// First call: send new message.
	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	if backend.sendCount() != 1 {
		t.Fatalf("send count = %d, want 1", backend.sendCount())
	}
	if backend.editCount() != 0 {
		t.Fatalf("edit count = %d, want 0", backend.editCount())
	}

	// Second call: edit existing message.
	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if backend.sendCount() != 1 {
		t.Errorf("send count = %d, want 1 (should edit, not send)", backend.sendCount())
	}
	if backend.editCount() != 1 {
		t.Errorf("edit count = %d, want 1", backend.editCount())
	}
}

func TestCleanupPreview_DeletesInPreviewMode(t *testing.T) {
	// Verifies that CleanupPreview deletes the message when in preview mode,
	// and is a no-op in full mode or when no message exists.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "preview"}, noHint)

	// No message -> no delete.
	tracker.CleanupPreview()
	if backend.deleteCount() != 0 {
		t.Errorf("delete count = %d, want 0 (no message to clean)", backend.deleteCount())
	}

	// Send a message, then cleanup.
	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.CleanupPreview()
	if backend.deleteCount() != 1 {
		t.Errorf("delete count = %d, want 1", backend.deleteCount())
	}

	// After cleanup, msgID is cleared -> second call is no-op.
	tracker.CleanupPreview()
	if backend.deleteCount() != 1 {
		t.Errorf("delete count = %d, want 1 (idempotent)", backend.deleteCount())
	}
}

func TestCleanupPreview_NoDeleteInFullMode(t *testing.T) {
	// Verifies that CleanupPreview does not delete messages in full mode.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.CleanupPreview()
	if backend.deleteCount() != 0 {
		t.Errorf("delete count = %d, want 0 (full mode should not delete)", backend.deleteCount())
	}
}

func TestResetMsgID_ForcesNewMessage(t *testing.T) {
	// Verifies that after ResetMsgID, the next tool call in preview mode sends
	// a new message instead of editing the old one.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "preview"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	if backend.sendCount() != 1 {
		t.Fatalf("send count = %d, want 1", backend.sendCount())
	}

	tracker.ResetMsgID()

	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if backend.sendCount() != 2 {
		t.Errorf("send count = %d, want 2 (should send new after reset)", backend.sendCount())
	}
	if backend.editCount() != 0 {
		t.Errorf("edit count = %d, want 0 (should not edit after reset)", backend.editCount())
	}
}

func TestObserveToolResult_IgnoredWhenNotFull(t *testing.T) {
	// Verifies that ObserveToolResult is a no-op when not in full mode.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "preview"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.ObserveToolResult("shell", "file1\nfile2", false)

	if backend.editWithButtonCount() != 0 {
		t.Error("preview mode should not process tool results")
	}
}

func TestObserveToolResult_UpdatesHint(t *testing.T) {
	// Verifies that when a hint function returns a non-empty hint, the compact
	// notification is updated via EditWithButton.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	hintFn := func(toolName string, params json.RawMessage, result string) string {
		return "42 lines"
	}
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, hintFn)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.ObserveToolResult("shell", "lots of output", false)

	if backend.editWithButtonCount() != 1 {
		t.Fatalf("editWithButton count = %d, want 1", backend.editWithButtonCount())
	}
	eb := backend.editWithButtons[0]
	if eb.btnLabel != "Show full" {
		t.Errorf("button label = %q, want Show full", eb.btnLabel)
	}
	// The text should contain the hint suffix.
	expected := "[compact] shell -> 42 lines"
	if eb.text != expected {
		t.Errorf("edit text = %q, want %q", eb.text, expected)
	}
}

func TestObserveToolResult_ExpandedUpdatesWithResult(t *testing.T) {
	// Verifies that when the entry is already expanded (user clicked "Show full"),
	// the result updates the expanded view.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	// Simulate user clicking "Show full" before result arrives.
	store.entries["msg-1"] = mockTrackerEntry{
		compact:  "[compact] shell",
		full:     "[full:full] shell {\"command\":\"ls\"}",
		result:   "",
		expanded: true,
	}

	tracker.ObserveToolResult("shell", "file1\nfile2", false)

	if backend.editWithButtonCount() != 1 {
		t.Fatalf("editWithButton count = %d, want 1", backend.editWithButtonCount())
	}
	eb := backend.editWithButtons[0]
	if eb.btnLabel != "Hide" {
		t.Errorf("button label = %q, want Hide", eb.btnLabel)
	}
}

func TestObserveToolResult_StoresAndPersists(t *testing.T) {
	// Verifies that ObserveToolResult updates both the in-memory store
	// and triggers persistence.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.ObserveToolResult("shell", "output", false)

	if len(store.persisted) != 1 {
		t.Fatalf("persisted count = %d, want 1", len(store.persisted))
	}
	p := store.persisted[0]
	if p.msgID != "msg-1" {
		t.Errorf("persisted msgID = %q, want msg-1", p.msgID)
	}
	if p.result != "output" {
		t.Errorf("persisted result = %q, want output", p.result)
	}
}

func TestObserveToolResult_NoMsgID(t *testing.T) {
	// Verifies that ObserveToolResult is a no-op when there is no current message ID
	// (e.g. tool call was suppressed due to send error).
	backend := &mockTrackerBackend{sendErr: fmt.Errorf("send failed")}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	tracker.ObserveToolResult("shell", "output", false)

	if backend.editWithButtonCount() != 0 {
		t.Error("should not edit when no message was sent")
	}
	if len(store.persisted) != 0 {
		t.Error("should not persist when no message was sent")
	}
}

func TestNotifyRetry(t *testing.T) {
	// Verifies that NotifyRetry sends a retry notification and ClearRetryNotification
	// overwrites it.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.NotifyRetry("api.anthropic.com")
	if backend.sendCount() != 1 {
		t.Fatalf("send count = %d, want 1", backend.sendCount())
	}
	if backend.sends[0] != "retrying api.anthropic.com..." {
		t.Errorf("send text = %q", backend.sends[0])
	}

	tracker.ClearRetryNotification()
	if backend.editCount() != 1 {
		t.Fatalf("edit count = %d, want 1", backend.editCount())
	}
	if backend.edits[0].text != "request completed" {
		t.Errorf("edit text = %q", backend.edits[0].text)
	}
}

func TestClearRetryNotification_NoOp(t *testing.T) {
	// Verifies that ClearRetryNotification is a no-op when no retry was sent.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ClearRetryNotification()
	if backend.editCount() != 0 {
		t.Error("should not edit when no retry notification exists")
	}
}

func TestNotifyRetry_SendError(t *testing.T) {
	// Verifies that when the retry send fails, ClearRetryNotification is still safe.
	backend := &mockTrackerBackend{sendErr: fmt.Errorf("network error")}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.NotifyRetry("api.example.com")
	tracker.ClearRetryNotification()
	if backend.editCount() != 0 {
		t.Error("should not attempt edit when retry send failed")
	}
}

func TestFullMode_StoreEntryOnSend(t *testing.T) {
	// Verifies that in full mode, the store entry is created immediately on send
	// (before tool result arrives) so button callbacks can find it.
	backend := &mockTrackerBackend{}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "full"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))

	store.mu.Lock()
	entry, ok := store.entries["msg-1"]
	store.mu.Unlock()
	if !ok {
		t.Fatal("store entry not created on send")
	}
	if entry.result != "" {
		t.Errorf("entry result = %q, want empty (tool still running)", entry.result)
	}
	if entry.expanded {
		t.Error("entry should not be expanded initially")
	}
}

func TestPreviewMode_SendError(t *testing.T) {
	// Verifies that when send fails in preview mode, no message ID is stored
	// and subsequent calls still attempt to send (not edit).
	backend := &mockTrackerBackend{sendErr: fmt.Errorf("network error")}
	store := newMockTrackerStore()
	tracker := NewToolCallTracker(backend, store, TrackerDisplay{ShowToolCalls: "preview"}, noHint)

	tracker.ObserveToolCall("shell", json.RawMessage(`{"command":"ls"}`))
	if tracker.LastMsgID() != "" {
		t.Errorf("msgID = %q, want empty after send error", tracker.LastMsgID())
	}

	// Clear the error and try again -- should send, not edit.
	backend.sendErr = nil
	tracker.ObserveToolCall("read", json.RawMessage(`{"path":"foo.txt"}`))
	if backend.sendCount() != 2 {
		t.Errorf("send count = %d, want 2 (both attempts should send)", backend.sendCount())
	}
	if backend.editCount() != 0 {
		t.Error("should not edit when previous send failed")
	}
}
