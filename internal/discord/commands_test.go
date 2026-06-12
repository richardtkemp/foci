package discord

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/command"
	"foci/internal/dispatch"
)

// TestRenderCommandOutcomeNotHandled verifies NotHandled outcomes render
// nothing.
func TestRenderCommandOutcomeNotHandled(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/x"), &dispatch.CommandOutcome{NotHandled: true})
	if fs.sendCount() != 0 {
		t.Errorf("expected no sends, got %d", fs.sendCount())
	}
}

// TestRenderCommandOutcomeKeyboard verifies keyboard outcomes send the header
// with command buttons.
func TestRenderCommandOutcomeKeyboard(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/model"), &dispatch.CommandOutcome{
		Keyboard: &dispatch.KeyboardOutcome{
			CommandName: "model",
			Header:      "Pick a model:",
			Options:     []command.KeyboardOption{{Label: "Haiku", Data: "haiku"}},
		},
	})
	got := fs.lastSend(t)
	if got.content != "Pick a model:" || len(got.components) != 1 {
		t.Errorf("expected header with buttons, got %+v", got)
	}
}

// TestRenderCommandOutcomeChain verifies chain outcomes send the label with
// follow-up buttons.
func TestRenderCommandOutcomeChain(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/branch"), &dispatch.CommandOutcome{
		Chain: &dispatch.ChainOutcome{
			CommandName: "branch",
			Label:       "Choose a branch:",
			Options:     []command.KeyboardOption{{Label: "main", Data: "main"}},
		},
	})
	got := fs.lastSend(t)
	if got.content != "Choose a branch:" || len(got.components) != 1 {
		t.Errorf("expected chain label with buttons, got %+v", got)
	}
}

// TestRenderCommandOutcomeResponseText verifies a plain text response is sent
// as a reply to the originating channel.
func TestRenderCommandOutcomeResponseText(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/ver"), &dispatch.CommandOutcome{
		Response: &dispatch.ResponseOutcome{
			Result: dispatch.Result{Handled: true, Response: command.Response{Text: "v1"}},
		},
	})
	got := fs.lastSend(t)
	if got.channelID != "42" || got.content != "v1" {
		t.Errorf("unexpected reply %+v", got)
	}
}

// TestRenderCommandOutcomeResponseParts verifies multi-part responses are sent
// as separate messages in order.
func TestRenderCommandOutcomeResponseParts(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/list"), &dispatch.CommandOutcome{
		Response: &dispatch.ResponseOutcome{
			Result: dispatch.Result{Handled: true, Response: command.Response{Parts: []string{"one", "two"}}},
		},
	})
	if fs.sendCount() != 2 {
		t.Fatalf("expected 2 part sends, got %d", fs.sendCount())
	}
	if fs.sends[0].content != "one" || fs.sends[1].content != "two" {
		t.Errorf("parts out of order: %q, %q", fs.sends[0].content, fs.sends[1].content)
	}
}

// TestRenderCommandOutcomeResponseKeyboard verifies responses carrying a
// keyboard are sent with buttons derived from the looked-up command name.
func TestRenderCommandOutcomeResponseKeyboard(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.renderCommandOutcome(testDiscordMessage("42", "u", "/model"), &dispatch.CommandOutcome{
		Response: &dispatch.ResponseOutcome{
			LookupText: "/model",
			Result: dispatch.Result{Handled: true, Response: command.Response{
				Text:     "Current: haiku",
				Keyboard: []command.KeyboardOption{{Label: "Opus", Data: "opus"}},
			}},
		},
	})
	got := fs.lastSend(t)
	if got.content != "Current: haiku" || len(got.components) != 1 {
		t.Errorf("expected response with keyboard, got %+v", got)
	}
}

// TestRenderCommandOutcomeDocPath verifies document responses send the file
// and remove it afterwards.
func TestRenderCommandOutcomeDocPath(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	doc := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(doc, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	b.renderCommandOutcome(testDiscordMessage("42", "u", "/export"), &dispatch.CommandOutcome{
		Response: &dispatch.ResponseOutcome{
			Result: dispatch.Result{Handled: true, Response: command.Response{DocPath: doc}},
		},
	})
	got := fs.lastSend(t)
	if len(got.fileNames) != 1 || got.fileNames[0] != "out.txt" {
		t.Errorf("expected document attachment, got %v", got.fileNames)
	}
	if _, err := os.Stat(doc); !os.IsNotExist(err) {
		t.Error("expected document removed after send")
	}
}
