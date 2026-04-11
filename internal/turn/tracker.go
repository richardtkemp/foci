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

// previewKey is the sentinel map key used for the single shared entry in
// preview mode, where all tool calls edit one message. Real tool_use IDs are
// always non-empty so there's no risk of collision.
const previewKey = ""

// trackerEntry holds per-tool-call state for the tool tracker. Each entry in
// the tracker's entries map owns one message (full mode) or the shared
// preview message (preview mode, via previewKey).
type trackerEntry struct {
	msgID      string
	text       string           // compact summary (full mode) or full text (preview mode)
	fullText   string           // full-formatted tool call text (full mode only)
	lastParams json.RawMessage  // params of the originating tool_use (for result hints)
}

// ToolCallTracker manages tool call visibility state during an agent turn.
// It encapsulates the mutable state shared between ToolCall and ToolResult
// events fired from the delegator via StreamingSink.
//
// State is keyed by tool_use_id so parallel tool calls (Claude often batches
// multiple tool_use blocks in a single assistant message) don't clobber each
// other — each tool gets its own entry and its result updates the correct
// message. Preview mode shares a single entry via the previewKey sentinel
// since preview-mode UX is "one shared message that edits in place".
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
	entries    map[string]*trackerEntry
	order      []string // insertion order of entry keys; LastMsgID reads order[len-1]
	retryMsgID string   // retry notification message ID (separate from tool-call entries)
}

// NewToolCallTracker creates a new shared tool call tracker.
func NewToolCallTracker(backend TrackerBackend, store TrackerStore, display TrackerDisplay, hintFunc CompactResultHintFunc) *ToolCallTracker {
	return &ToolCallTracker{
		backend:  backend,
		store:    store,
		display:  display,
		hintFunc: hintFunc,
		entries:  make(map[string]*trackerEntry),
	}
}

// LastMsgID returns the message ID of the most recently recorded tool-call
// entry. Used by the renderer's preview-mode code paths to locate the
// in-place-edit target when a reply arrives.
func (t *ToolCallTracker) LastMsgID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.order) == 0 {
		return ""
	}
	e, ok := t.entries[t.order[len(t.order)-1]]
	if !ok {
		return ""
	}
	return e.msgID
}

// ResetMsgID clears all recorded entries. Called from the renderer after an
// intermediate reply is delivered — the next tool call starts fresh
// (preview mode sends a new message rather than editing the previous one;
// full mode has no in-flight correlation to worry about since results were
// already handled through ObserveToolResult).
func (t *ToolCallTracker) ResetMsgID() {
	t.mu.Lock()
	t.entries = make(map[string]*trackerEntry)
	t.order = nil
	t.mu.Unlock()
}

// CleanupPreview deletes the preview-mode shared message if one exists.
// Called when the final response is delivered via a separate message
// (streaming, thinking, or long response) so the transient tool call
// doesn't linger in chat. No-op outside preview mode.
func (t *ToolCallTracker) CleanupPreview() {
	t.mu.Lock()
	var id string
	if e, ok := t.entries[previewKey]; ok {
		id = e.msgID
		delete(t.entries, previewKey)
		t.removeOrder(previewKey)
	}
	t.mu.Unlock()
	if id == "" || t.display.ShowToolCalls != "preview" {
		return
	}
	_ = t.backend.Delete(id)
}

// ObserveToolCall handles tool call visibility via send+edit pattern.
// The id is the Anthropic tool_use_id (or the delegator's equivalent) —
// it's the key used to correlate with the matching ObserveToolResult.
func (t *ToolCallTracker) ObserveToolCall(id, toolName string, params json.RawMessage) {
	mode := t.display.ShowToolCalls
	if mode == "off" || mode == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if mode == "full" {
		t.sendFullModeToolCall(id, toolName, params)
		return
	}
	t.sendPreviewModeToolCall(toolName, params)
}

// sendFullModeToolCall sends a compact summary with a "Show full" button.
// Each tool call gets its own message and its own entry keyed by the real
// tool_use id. Called under lock.
func (t *ToolCallTracker) sendFullModeToolCall(id, toolName string, params json.RawMessage) {
	compact := t.backend.FormatCompact(toolName, params)
	full := t.backend.FormatFull(toolName, params, t.display.ShowToolCalls)
	msgID, err := t.backend.SendWithButton(compact, "Show full", "tc:show")
	if err != nil {
		t.backend.Logger().Debugf("send tool call msg: %v", err)
		return
	}
	t.setEntry(id, &trackerEntry{
		msgID:      msgID,
		text:       compact,
		fullText:   full,
		lastParams: params,
	})
	t.store.StoreEntry(msgID, compact, full, "", false)
}

// sendPreviewModeToolCall sends or edits the single shared preview message
// (keyed by previewKey). Overwriting-in-place preserves the existing "one
// preview message that updates as new tools fire" UX. Called under lock.
func (t *ToolCallTracker) sendPreviewModeToolCall(toolName string, params json.RawMessage) {
	text := t.backend.FormatFull(toolName, params, t.display.ShowToolCalls)
	e, ok := t.entries[previewKey]
	if !ok || e.msgID == "" {
		msgID, err := t.backend.Send(text)
		if err != nil {
			t.backend.Logger().Debugf("send tool call msg: %v", err)
			return
		}
		t.setEntry(previewKey, &trackerEntry{msgID: msgID, text: text})
		return
	}
	if err := t.backend.Edit(e.msgID, text); err != nil {
		t.backend.Logger().Debugf("edit tool call msg: %v", err)
	}
	e.text = text
}

// ObserveToolResult stores tool results for inline keyboard expansion (full
// mode only). Looks up the per-tool entry by id so parallel tool calls each
// get their own result hint applied to the correct message. When a hint is
// available, the compact notification is updated inline (e.g. "shell: ls"
// becomes "shell: ls -> 42 lines").
func (t *ToolCallTracker) ObserveToolResult(id, toolName, result string, isError bool) {
	if t.display.ShowToolCalls != "full" {
		return
	}
	t.mu.Lock()
	e, ok := t.entries[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	msgID := e.msgID
	compact := e.text
	full := e.fullText
	params := e.lastParams
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

// setEntry inserts or updates an entry, maintaining insertion order so
// LastMsgID returns the most recently added entry. Called under lock.
func (t *ToolCallTracker) setEntry(key string, e *trackerEntry) {
	if _, exists := t.entries[key]; !exists {
		t.order = append(t.order, key)
	}
	t.entries[key] = e
}

// removeOrder drops key from the order slice in place. Called under lock.
func (t *ToolCallTracker) removeOrder(key string) {
	for i, k := range t.order {
		if k == key {
			t.order = append(t.order[:i], t.order[i+1:]...)
			return
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
