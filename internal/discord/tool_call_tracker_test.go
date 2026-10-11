package discord

import (
	"encoding/json"
	"strings"
	"testing"

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

// TestFormatToolCallCompact_LoadedSkill proves a Read of a SKILL.md renders
// the whole compact line as the "📖 Loaded skill: <dir>" chip (#1299) for
// both read forms that reach the formatter — ccstream's Read/file_path and
// the API transport's read/path — and that the chip is exclusive to
// SKILL.md: a sub-file inside a skill directory and reads of ordinary files
// (both read forms) keep the exact pre-#1299 summaries.
func TestFormatToolCallCompact_LoadedSkill(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		params string
		want   string
	}{
		{"ccstream Read form", "Read", `{"file_path":"/home/foci/shared/skills/writing-code/SKILL.md"}`, "📖 Loaded skill: writing-code"},
		{"API read form", "read", `{"path":"/home/foci/shared/skills/writing-code/SKILL.md"}`, "📖 Loaded skill: writing-code"},
		{"sub-file inside skill dir keeps generic summary", "read", `{"path":"/home/foci/shared/skills/writing-code/go.md"}`, "[] **read**: /home/foci/shared/skills/writing-code/go.md"},
		{"ordinary read keeps generic summary", "read", `{"path":"/home/foci/notes.txt"}`, "[] **read**: /home/foci/notes.txt"},
		{"ordinary CC Read keeps generic summary", "Read", `{"file_path":"/home/foci/notes.txt"}`, "## **Read**: /home/foci/notes.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatToolCallCompact(tt.tool, json.RawMessage(tt.params)); got != tt.want {
				t.Errorf("formatToolCallCompact(%s) = %q, want %q", tt.tool, got, tt.want)
			}
		})
	}
}

// TestTrackerFullMode_StoresLoadedSkillLine proves a SKILL.md read flowing
// through the full tracker chain — the API transport's verbatim read/path —
// lands in chat and in the stored entry as the chip: the sent message and
// CompactText are the chip line, and FullInput still carries the real path
// (#1299).
func TestTrackerFullMode_StoresLoadedSkillLine(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	tracker := newToolCallTracker(b, "42", turn.TurnDisplay{ShowToolCalls: "full"})

	const skillPath = "/home/foci/shared/skills/writing-code/SKILL.md"
	tracker.ObserveToolCall("t1", "read", json.RawMessage(`{"path":"`+skillPath+`"}`))

	const chip = "📖 Loaded skill: writing-code"
	if got := fs.sendCount(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if got := fs.lastSend(t).content; got != chip {
		t.Errorf("sent text = %q, want %q", got, chip)
	}
	e, ok := b.toolStore.Load("1")
	if !ok {
		t.Fatal("no stored entry for message 1")
	}
	if e.CompactText != chip {
		t.Errorf("CompactText = %q, want %q", e.CompactText, chip)
	}
	if !strings.Contains(e.FullInput, skillPath) {
		t.Errorf("FullInput lost the real path: %q", e.FullInput)
	}
}
