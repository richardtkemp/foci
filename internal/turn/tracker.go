package turn

import (
	"encoding/json"
	"sync"

	"foci/internal/log"
)

// TrackerBackend abstracts platform-specific operations for the tool call tracker.
// Each platform (Telegram, Discord) implements this with its own formatting and API calls.
type TrackerBackend interface {
	// FormatCompact returns a compact one-line summary for a tool call.
	FormatCompact(toolName string, params json.RawMessage) string
	// FormatFull returns full tool call display text.
	FormatFull(toolName string, params json.RawMessage, showMode string) string
	// FormatWithResult combines tool text with result, truncated to fit platform limits.
	FormatWithResult(toolText, result string) string
	// FormatHintSuffix returns the hint appended to compact text (e.g. " -> hint" or " -> hint").
	FormatHintSuffix(hint string) string
	// FormatRetry returns retry notification text.
	FormatRetry(endpoint string) string
	// FormatRetryClear returns retry-cleared text.
	FormatRetryClear() string

	// Send sends a message, returns its ID.
	Send(text string) (msgID string, err error)
	// SendWithButton sends a message with a single button, returns its ID.
	SendWithButton(text, btnLabel, btnData string) (msgID string, err error)
	// Edit edits a message's text.
	Edit(msgID, text string) error
	// EditWithButton edits a message's text and replaces its button.
	EditWithButton(msgID, text, btnLabel, btnData string) error
	// Delete deletes a message.
	Delete(msgID string) error

	// Logger returns the component logger.
	Logger() *log.ComponentLogger
}

// TrackerStore handles tool result entry persistence.
type TrackerStore interface {
	// StoreEntry stores a tool result entry (in-memory, e.g. sync.Map).
	StoreEntry(msgID, compact, full, result string, expanded bool)
	// IsExpanded returns whether the entry is in expanded state.
	IsExpanded(msgID string) bool
	// Persist writes the entry to durable storage (tooldetail DB).
	Persist(msgID, compact, full, result string)
}

// TrackerDisplay holds the resolved display configuration for the tracker.
type TrackerDisplay struct {
	ShowToolCalls string // "off", "preview", "full"
}

// CompactResultHintFunc is a function that extracts a short hint from a tool result.
// Each platform provides its own implementation (since the hint functions may
// differ in detail, though currently they are similar).
type CompactResultHintFunc func(toolName string, params json.RawMessage, result string) string

// ToolCallTracker manages tool call visibility state during an agent turn.
// It encapsulates the mutable state shared between ToolCallObserver and
// ToolResultObserver callbacks (message ID, text snapshots, mutex).
//
// Both Telegram and Discord trackers shared ~70% identical logic; this struct
// extracts the common state machine and delegates platform-specific formatting
// and messaging to TrackerBackend.
type ToolCallTracker struct {
	backend  TrackerBackend
	store    TrackerStore
	display  TrackerDisplay
	hintFunc CompactResultHintFunc

	mu         sync.Mutex
	msgID      string          // current tool-call message ID (string for both platforms)
	text       string          // last compact summary (full mode) or full text (preview mode)
	fullText   string          // last full formatted tool call text (full mode only)
	lastParams json.RawMessage // params of the last tool call (for result hints)
	retryMsgID string          // retry notification message ID
}

// NewToolCallTracker creates a new shared tool call tracker.
func NewToolCallTracker(backend TrackerBackend, store TrackerStore, display TrackerDisplay, hintFunc CompactResultHintFunc) *ToolCallTracker {
	return &ToolCallTracker{
		backend:  backend,
		store:    store,
		display:  display,
		hintFunc: hintFunc,
	}
}

// LastMsgID returns the current tool-call message ID (thread-safe).
func (t *ToolCallTracker) LastMsgID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgID
}

// ResetMsgID clears the tool-call message ID (e.g. after intermediate text).
func (t *ToolCallTracker) ResetMsgID() {
	t.mu.Lock()
	t.msgID = ""
	t.mu.Unlock()
}

// CleanupPreview deletes the tool call preview message if one exists.
// Called when the final response is delivered via a separate message (streaming,
// thinking, or long response) so the transient tool call doesn't linger in chat.
func (t *ToolCallTracker) CleanupPreview() {
	t.mu.Lock()
	id := t.msgID
	t.msgID = ""
	t.mu.Unlock()
	if id == "" || t.display.ShowToolCalls != "preview" {
		return
	}
	_ = t.backend.Delete(id)
}

// ObserveToolCall handles tool call visibility via send+edit pattern.
func (t *ToolCallTracker) ObserveToolCall(toolName string, params json.RawMessage) {
	mode := t.display.ShowToolCalls
	if mode == "off" || mode == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if mode == "full" {
		t.sendFullModeToolCall(toolName, params)
		return
	}
	t.sendPreviewModeToolCall(toolName, params)
}

// sendFullModeToolCall sends a compact summary with a "Show full" button.
// Called under lock.
func (t *ToolCallTracker) sendFullModeToolCall(toolName string, params json.RawMessage) {
	compact := t.backend.FormatCompact(toolName, params)
	full := t.backend.FormatFull(toolName, params, t.display.ShowToolCalls)
	msgID, err := t.backend.SendWithButton(compact, "Show full", "tc:show")
	if err != nil {
		t.backend.Logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.msgID = msgID
	t.text = compact
	t.fullText = full
	t.lastParams = params
	t.store.StoreEntry(msgID, compact, full, "", false)
}

// sendPreviewModeToolCall sends or edits a tool call message (overwriting previous).
// Called under lock.
func (t *ToolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := t.backend.FormatFull(toolName, params, t.display.ShowToolCalls)
	if t.msgID == "" {
		msgID, err := t.backend.Send(text)
		if err != nil {
			t.backend.Logger().Debugf("send tool call msg: %v", err)
			return
		}
		t.msgID = msgID
		t.text = text
	} else {
		if err := t.backend.Edit(t.msgID, text); err != nil {
			t.backend.Logger().Debugf("edit tool call msg: %v", err)
		}
		t.text = text
	}
}

// ObserveToolResult stores tool results for inline keyboard expansion (full mode only).
// When a result hint is available, the compact notification is updated inline
// (e.g. "shell: ls" becomes "shell: ls -> 42 lines").
func (t *ToolCallTracker) ObserveToolResult(toolName string, result string, isError bool) {
	if t.display.ShowToolCalls != "full" {
		return
	}
	t.mu.Lock()
	msgID := t.msgID
	compact := t.text
	full := t.fullText
	params := t.lastParams
	t.mu.Unlock()
	if msgID == "" {
		return
	}

	// Generate a result hint to append to the compact notification.
	hint := t.hintFunc(toolName, params, result)
	if hint != "" {
		compact = compact + t.backend.FormatHintSuffix(hint)
	}

	wasExpanded := t.store.IsExpanded(msgID)

	t.store.StoreEntry(msgID, compact, full, result, wasExpanded)
	t.store.Persist(msgID, compact, full, result)

	if wasExpanded {
		expanded := t.backend.FormatWithResult(full, result)
		if err := t.backend.EditWithButton(msgID, expanded, "Hide", "tc:hide"); err != nil {
			t.backend.Logger().Debugf("edit expanded tool result: %v", err)
		}
	} else if hint != "" {
		// Update the compact notification with the result hint.
		if err := t.backend.EditWithButton(msgID, compact, "Show full", "tc:show"); err != nil {
			t.backend.Logger().Debugf("edit tool hint: %v", err)
		}
	}
}

// NotifyRetry sends a retry notification message on first API retry.
func (t *ToolCallTracker) NotifyRetry(endpoint string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	text := t.backend.FormatRetry(endpoint)
	msgID, err := t.backend.Send(text)
	if err != nil {
		t.backend.Logger().Debugf("send retry notification: %v", err)
		return
	}
	t.retryMsgID = msgID
}

// ClearRetryNotification overwrites the retry notification on success.
func (t *ToolCallTracker) ClearRetryNotification() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.retryMsgID == "" {
		return
	}

	text := t.backend.FormatRetryClear()
	if err := t.backend.Edit(t.retryMsgID, text); err != nil {
		t.backend.Logger().Debugf("clear retry notification: %v", err)
	}

	t.retryMsgID = ""
}

