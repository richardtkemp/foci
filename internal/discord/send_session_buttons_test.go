package discord

import (
	"testing"

	"foci/internal/platform"
)

// sessionPromptButtons is the shared minimal button set for the
// session-addressed prompt tests.
func sessionPromptButtons() []platform.ButtonChoice {
	return []platform.ButtonChoice{{Label: "Yes", Data: "yes"}, {Label: "No", Data: "no"}}
}

// TestSendTextWithButtonsToSession_RoutesToSessionChannel proves #2275: a
// session-addressed prompt send targets the channel embedded in the session
// key, NOT the default channel — and falls back to the default for a chatless
// key (agent/i<name>). The last arm pins the "no channel at all" error.
func TestSendTextWithButtonsToSession_RoutesToSessionChannel(t *testing.T) {
	b, fs, idx := newTestBot(t, "main")
	if err := idx.SetDefaultChat("main", platformName, 999); err != nil {
		t.Fatal(err)
	}

	if _, err := b.SendTextWithButtonsToSession("main/c222", "Q?", sessionPromptButtons(), "im:"); err != nil {
		t.Fatalf("SendTextWithButtonsToSession: %v", err)
	}
	if got := fs.lastSend(t); got.channelID != "222" {
		t.Errorf("routed to channel %q, want 222 (the session's channel, not the default 999)", got.channelID)
	}

	if _, err := b.SendTextWithButtonsToSession("main/iwork", "Q?", sessionPromptButtons(), "im:"); err != nil {
		t.Fatalf("SendTextWithButtonsToSession (chatless key): %v", err)
	}
	if got := fs.lastSend(t); got.channelID != "999" {
		t.Errorf("routed to channel %q, want 999 (default fallback for a chatless key)", got.channelID)
	}

	bare, _, _ := newTestBot(t, "main") // no default, no last channel
	if _, err := bare.SendTextWithButtonsToSession("main/iwork", "Q?", sessionPromptButtons(), "im:"); err == nil {
		t.Error("expected an error when no channel can be resolved (chatless key, no default and no last channel)")
	}
}

// TestEditMessageTextInSession_TargetsSessionChannel proves the
// button-stripping edit addresses the session's channel (an edit addressing a
// msgID from another channel fails) (#2275).
func TestEditMessageTextInSession_TargetsSessionChannel(t *testing.T) {
	b, fs, idx := newTestBot(t, "main")
	if err := idx.SetDefaultChat("main", platformName, 999); err != nil {
		t.Fatal(err)
	}

	if err := b.EditMessageTextInSession("main/c222", "7", "✅ done"); err != nil {
		t.Fatalf("EditMessageTextInSession: %v", err)
	}
	if got := fs.lastEdit(t); got.channelID != "222" {
		t.Errorf("edit targeted channel %q, want 222 (the session's channel)", got.channelID)
	}

	if err := b.EditMessageTextInSession("main/iwork", "7", "✅ done"); err != nil {
		t.Fatalf("EditMessageTextInSession (chatless key): %v", err)
	}
	if got := fs.lastEdit(t); got.channelID != "999" {
		t.Errorf("edit targeted channel %q, want 999 (default fallback for a chatless key)", got.channelID)
	}
}

// TestEditMessageWithButtonsInSession_TargetsSessionChannel proves the
// keyboard-replacing edit addresses the session's channel and keeps the
// buttons (#2275).
func TestEditMessageWithButtonsInSession_TargetsSessionChannel(t *testing.T) {
	b, fs, idx := newTestBot(t, "main")
	if err := idx.SetDefaultChat("main", platformName, 999); err != nil {
		t.Fatal(err)
	}

	if err := b.EditMessageWithButtonsInSession("main/c222", "7", "again?", sessionPromptButtons(), "im:"); err != nil {
		t.Fatalf("EditMessageWithButtonsInSession: %v", err)
	}
	got := fs.lastEdit(t)
	if got.channelID != "222" {
		t.Errorf("edit targeted channel %q, want 222 (the session's channel)", got.channelID)
	}
	if len(got.components) != 1 {
		t.Errorf("expected the replacement keyboard (1 action row), got %d rows", len(got.components))
	}
}
