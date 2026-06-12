package discord

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/tooldetail"
	"foci/internal/turn"
)

// TestEmojiForTool verifies known tools get their prefix and unknown tools the
// generic fallback.
func TestEmojiForTool(t *testing.T) {
	if got := emojiForTool("shell"); got != "> " {
		t.Errorf("shell: got %q", got)
	}
	if got := emojiForTool("never-heard-of-it"); got != "## " {
		t.Errorf("unknown: got %q", got)
	}
}

// TestFormatToolCallCompact verifies the compact one-liner for valid params,
// invalid JSON, and params without a summary.
func TestFormatToolCallCompact(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		params string
		want   string // substring expectations checked below
	}{
		{"shell command", "shell", `{"command": "ls -la"}`, "ls -la"},
		{"invalid json falls back to name", "shell", `{invalid`, "**shell**"},
		{"no summary falls back to name", "shell", `{}`, "**shell**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatToolCallCompact(tt.tool, json.RawMessage(tt.params))
			if !strings.Contains(got, tt.want) {
				t.Errorf("got %q, want substring %q", got, tt.want)
			}
		})
	}
}

// TestFormatToolCallFull verifies pretty-printed JSON, preview truncation, and
// that "full" mode skips truncation.
func TestFormatToolCallFull(t *testing.T) {
	long := strings.Repeat("x", 600)
	params := json.RawMessage(`{"data": "` + long + `"}`)

	preview := formatToolCallFull("shell", params, "preview", 100)
	if !strings.Contains(preview, "...") {
		t.Error("expected preview truncation marker")
	}
	if !strings.Contains(preview, "```json") {
		t.Error("expected JSON code fence")
	}

	full := formatToolCallFull("shell", params, "full", 100)
	if strings.Contains(full, "...") && !strings.Contains(full, long) {
		t.Error("full mode should not truncate")
	}

	// Default maxChars (0 -> 450) still truncates the 600-char payload.
	def := formatToolCallFull("shell", params, "preview", 0)
	if !strings.Contains(def, "...") {
		t.Error("expected default 450-char truncation")
	}
}

// TestFormatToolCallWithResultOverhead verifies that a tool text already at the
// message limit is returned unmodified (no room for the result).
func TestFormatToolCallWithResultOverhead(t *testing.T) {
	huge := strings.Repeat("y", discordMaxChars)
	if got := formatToolCallWithResult(huge, "result"); got != huge {
		t.Error("expected tool text returned unchanged when no room for result")
	}
}

// TestTrackerBackendFormatting verifies the string-formatting members of the
// Discord tracker backend.
func TestTrackerBackendFormatting(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	backend := &discordTrackerBackend{bot: b, channelID: "42"}

	if got := backend.FormatHintSuffix("3 files"); got != " -> 3 files" {
		t.Errorf("hint: got %q", got)
	}
	if got := backend.FormatRetry("api.anthropic.com"); !strings.Contains(got, "api.anthropic.com") {
		t.Errorf("retry: got %q", got)
	}
	if backend.FormatRetryClear() == "" {
		t.Error("expected non-empty retry-clear text")
	}
	if got := backend.FormatCompact("shell", json.RawMessage(`{"command":"ls"}`)); !strings.Contains(got, "shell") {
		t.Errorf("compact: got %q", got)
	}
	if got := backend.FormatFull("shell", json.RawMessage(`{"command":"ls"}`), "full"); !strings.Contains(got, "```json") {
		t.Errorf("full: got %q", got)
	}
	if got := backend.FormatWithResult("**shell**", "ok"); !strings.Contains(got, "ok") {
		t.Errorf("with result: got %q", got)
	}
	if backend.Logger() == nil {
		t.Error("expected logger")
	}
}

// TestTrackerBackendMessaging verifies Send/SendWithButton/Edit/EditWithButton/
// Delete are routed through the message API with button components attached.
func TestTrackerBackendMessaging(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	backend := &discordTrackerBackend{bot: b, channelID: "42"}

	id, err := backend.Send("tool text")
	if err != nil || id == "" {
		t.Fatalf("Send: id=%q err=%v", id, err)
	}
	if got := fs.lastSend(t); got.content != "tool text" || got.channelID != "42" {
		t.Errorf("Send: %+v", got)
	}

	id2, err := backend.SendWithButton("with button", "Show full", "tc:show")
	if err != nil || id2 == "" {
		t.Fatalf("SendWithButton: id=%q err=%v", id2, err)
	}
	if got := fs.lastSend(t); len(got.components) != 1 {
		t.Error("SendWithButton: expected button components")
	}

	if err := backend.Edit(id, "edited"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastEdit(t); got.content != "edited" || got.msgID != id {
		t.Errorf("Edit: %+v", got)
	}

	if err := backend.EditWithButton(id, "edited btn", "Hide", "tc:hide"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastEdit(t); len(got.components) != 1 {
		t.Error("EditWithButton: expected button components")
	}

	if err := backend.Delete(id); err != nil {
		t.Fatal(err)
	}
	if len(fs.deletes) != 1 || fs.deletes[0] != id {
		t.Errorf("Delete: %v", fs.deletes)
	}
}

// TestTrackerStore verifies StoreEntry/IsExpanded round-trips through the bot's
// in-memory map and Persist writes through to the SQLite store when present.
func TestTrackerStore(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	store := &discordTrackerStore{bot: b, channelID: "42"}

	if store.IsExpanded("100") {
		t.Error("expected unknown entry not expanded")
	}
	store.StoreEntry("100", "compact", "full", "result", true)
	if !store.IsExpanded("100") {
		t.Error("expected stored entry expanded")
	}

	// Persist without a detail store is a no-op.
	store.Persist("100", "c", "f", "r")

	// Persist with a real store writes through.
	td, err := tooldetail.NewStore(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = td.Close() }()
	b.toolDetailStore = td
	store.Persist("100", "c2", "f2", "r2")
	entries, err := td.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entries[100]; !ok || e.CompactText != "c2" {
		t.Errorf("expected persisted entry, got %+v", entries)
	}
}

// TestNewToolCallTracker verifies tracker construction wires the Discord
// backend and store.
func TestNewToolCallTracker(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	tracker := newToolCallTracker(b, "42", turn.TurnDisplay{ShowToolCalls: "preview"})
	if tracker == nil {
		t.Fatal("expected tracker")
	}
}

// TestCompactResultHint verifies the hint delegate routes to toolformat (shell
// gets a hint, unknown tools get none); detailed hint formats are covered in
// toolformat's own tests.
func TestCompactResultHint(t *testing.T) {
	if got := compactResultHint("shell", json.RawMessage(`{"command":"ls"}`), "a\nb\nc\nd\ne"); got != "5 lines" {
		t.Errorf("expected '5 lines' hint for multi-line shell result, got %q", got)
	}
	if got := compactResultHint("mystery", json.RawMessage(`{}`), "x"); got != "" {
		t.Errorf("expected no hint for unknown tool, got %q", got)
	}
}
