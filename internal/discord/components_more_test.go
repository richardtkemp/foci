package discord

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"

	"github.com/bwmarrin/discordgo"
)

// storeToolResult seeds the bot's in-memory tool result map for callback tests.
func storeToolResult(b *Bot, msgID int64, entry toolResultEntry) {
	b.toolResults.Store(msgID, entry)
}

// componentInteraction builds a button-press interaction event.
func componentInteraction(channelID, msgID, customID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type:      discordgo.InteractionMessageComponent,
			ChannelID: channelID,
			Data:      discordgo.MessageComponentInteractionData{CustomID: customID},
			Message:   &discordgo.Message{ID: msgID},
		},
	}
}

// TestHandleToolCallCallbackShow verifies the "show" button expands a stored
// tool call (with a Running placeholder before the result arrives) and swaps
// the button to Hide.
func TestHandleToolCallCallbackShow(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	storeToolResult(b, 100, toolResultEntry{compactText: "compact", fullInput: "full input"})

	b.handleToolCallCallback("42", "show", "100")

	got := fs.lastEdit(t)
	if !strings.Contains(got.content, "full input") || !strings.Contains(got.content, "Running...") {
		t.Errorf("expected expanded view with Running placeholder, got %q", got.content)
	}
	val, _ := b.toolResults.Load(int64(100))
	if !val.(toolResultEntry).expanded {
		t.Error("expected entry marked expanded")
	}

	// With a result present, the expansion includes it.
	storeToolResult(b, 101, toolResultEntry{compactText: "c", fullInput: "f", result: "the result"})
	b.handleToolCallCallback("42", "show", "101")
	if got := fs.lastEdit(t); !strings.Contains(got.content, "the result") {
		t.Errorf("expected result in expansion, got %q", got.content)
	}
}

// TestHandleToolCallCallbackHide verifies the "hide" button collapses back to
// the compact text.
func TestHandleToolCallCallbackHide(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	storeToolResult(b, 100, toolResultEntry{compactText: "compact view", fullInput: "f", expanded: true})

	b.handleToolCallCallback("42", "hide", "100")

	got := fs.lastEdit(t)
	if got.content != "compact view" {
		t.Errorf("expected compact text, got %q", got.content)
	}
	val, _ := b.toolResults.Load(int64(100))
	if val.(toolResultEntry).expanded {
		t.Error("expected entry marked collapsed")
	}
}

// TestHandleToolCallCallbackUnknownMessage verifies unknown message IDs are
// ignored without edits.
func TestHandleToolCallCallbackUnknownMessage(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.handleToolCallCallback("42", "show", "999")
	if len(fs.edits) != 0 {
		t.Errorf("expected no edits, got %d", len(fs.edits))
	}
}

// TestHandleThinkingCallback verifies the thinking show/hide toggle edits the
// message between expanded (thinking + response) and collapsed (response only).
func TestHandleThinkingCallback(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.display.DisplayWidth = 20
	b.thinkingStore.Store(int64(100), thinkingEntry{responseText: "the answer", thinkingText: "deep thought"})

	b.handleThinkingCallback("42", "show", "100")
	got := fs.lastEdit(t)
	if !strings.Contains(got.content, "deep thought") || !strings.Contains(got.content, "the answer") {
		t.Errorf("expected expanded thinking, got %q", got.content)
	}

	b.handleThinkingCallback("42", "hide", "100")
	if got := fs.lastEdit(t); got.content != "the answer" {
		t.Errorf("expected collapsed response, got %q", got.content)
	}

	// Unknown message: no edit.
	before := len(fs.edits)
	b.handleThinkingCallback("42", "show", "999")
	if len(fs.edits) != before {
		t.Error("expected no edit for unknown message")
	}
}

// TestHandleComponentInteraction verifies button interactions are acknowledged
// and dispatched by their callback prefix (tool-call expansion here).
func TestHandleComponentInteraction(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	storeToolResult(b, 100, toolResultEntry{compactText: "c", fullInput: "full input"})

	b.handleComponentInteraction(context.Background(), componentInteraction("42", "100", "tc:show"))

	if fs.interactionResponds != 1 {
		t.Errorf("expected interaction acknowledged, got %d", fs.interactionResponds)
	}
	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("expected tool expansion routed, got %q", got.content)
	}
}

// TestHandleComponentInteractionEmptyCustomID verifies empty custom IDs are
// ignored before acknowledgement.
func TestHandleComponentInteractionEmptyCustomID(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.handleComponentInteraction(context.Background(), componentInteraction("42", "100", ""))
	if fs.interactionResponds != 0 {
		t.Error("expected no acknowledgement for empty custom ID")
	}
}

// TestHandleCommandCallback verifies a command button press dispatches the
// command and edits the original message with the result.
func TestHandleCommandCallback(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	registerTestCommand(b, "ver", "v9")
	b.SetCommandContext(commandTestContext())

	b.handleCommandCallback(context.Background(), "42", "100", "/ver", 42)

	got := fs.lastEdit(t)
	if got.msgID != "100" || got.content != "v9" {
		t.Errorf("expected result edit, got %+v", got)
	}

	// Unknown command: edited with an error notice.
	b.handleCommandCallback(context.Background(), "42", "100", "/nope", 42)
	if got := fs.lastEdit(t); !strings.Contains(got.content, "Unknown command") {
		t.Errorf("expected unknown-command notice, got %q", got.content)
	}

	// Nil dispatcher: no edit.
	bare, fs2, _ := newTestBot(t, "a")
	bare.handleCommandCallback(context.Background(), "42", "100", "/ver", 42)
	if len(fs2.edits) != 0 {
		t.Error("expected no edit without dispatcher")
	}
}

// TestSendCommandKeyboard verifies the command keyboard helper sends header
// text with command-prefixed buttons to the default channel.
func TestSendCommandKeyboard(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.sendCommandKeyboard("model", "Pick:", []command.KeyboardOption{{Label: "Haiku", Data: "haiku"}})

	got := fs.lastSend(t)
	if got.content != "Pick:" || len(got.components) != 1 {
		t.Fatalf("expected header with one action row, got %+v", got)
	}
	row := got.components[0].(discordgo.ActionsRow)
	btn := row.Components[0].(discordgo.Button)
	if btn.CustomID != "cmd:/model haiku" {
		t.Errorf("unexpected button custom ID %q", btn.CustomID)
	}
}
