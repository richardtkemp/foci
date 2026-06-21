package discord

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/platform"
)

// TestSendMarkdownChunksSplitsLongText verifies that text over Discord's
// 2000-char limit is sent as multiple chunked messages whose concatenation
// preserves the content.
func TestSendMarkdownChunksSplitsLongText(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	long := strings.Repeat("line of text\n", 400) // ~5200 chars

	b.sendMarkdownChunks("42", long)

	if fs.sendCount() < 3 {
		t.Fatalf("expected at least 3 chunked sends, got %d", fs.sendCount())
	}
	var rebuilt strings.Builder
	for _, s := range fs.sends {
		if len(s.content) > discordMaxChars {
			t.Errorf("chunk exceeds limit: %d chars", len(s.content))
		}
		if s.channelID != "42" {
			t.Errorf("chunk sent to wrong channel %q", s.channelID)
		}
		rebuilt.WriteString(s.content)
	}
	if rebuilt.String() != long {
		t.Error("concatenated chunks do not reproduce original text")
	}
}

// TestSendMarkdownChunksStopsOnUnknownChannel verifies that a 10003 Unknown
// Channel error aborts remaining chunks and clears the stale default channel.
func TestSendMarkdownChunksStopsOnUnknownChannel(t *testing.T) {
	b, fs, idx := newTestBot(t, "a")
	if err := idx.SetDefaultChat("a", platformName, 42); err != nil {
		t.Fatal(err)
	}
	fs.sendErr = unknownChannelErr()

	b.sendMarkdownChunks("42", strings.Repeat("x\n", 3000))

	if fs.sendCount() != 0 {
		t.Errorf("expected no successful sends, got %d", fs.sendCount())
	}
	if got := idx.DefaultChatForAgent("a", platformName); got != 0 {
		t.Errorf("expected stale default channel cleared, got %d", got)
	}
}

// TestSendNotificationEmptySkipped verifies that empty and whitespace-only
// notifications are silently dropped.
func TestSendNotificationEmptySkipped(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)

	b.SendNotification("")
	b.SendNotification("   \n\t")

	if fs.sendCount() != 0 {
		t.Errorf("expected no sends for empty notifications, got %d", fs.sendCount())
	}
}

// TestSendNotificationBuffersDuringTurn verifies that notifications arriving
// during an active turn are buffered, then flushed in order by
// drainPendingNotifications.
func TestSendNotificationBuffersDuringTurn(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.turnActive.Store(true)

	b.SendNotification("first")
	b.SendNotification("second")
	if fs.sendCount() != 0 {
		t.Fatalf("expected notifications buffered, got %d sends", fs.sendCount())
	}

	b.turnActive.Store(false)
	b.drainPendingNotifications()

	if fs.sendCount() != 2 {
		t.Fatalf("expected 2 drained sends, got %d", fs.sendCount())
	}
	if fs.sends[0].content != "first" || fs.sends[1].content != "second" {
		t.Errorf("drain order wrong: %q, %q", fs.sends[0].content, fs.sends[1].content)
	}
}

// TestSendNotificationImmediate verifies that a notification outside a turn is
// sent straight to the default channel, and that the last-known channel is the
// fallback when no default is set.
func TestSendNotificationImmediate(t *testing.T) {
	b, fs, idx := newTestBot(t, "a")

	// No channel at all: dropped.
	b.SendNotification("lost")
	if fs.sendCount() != 0 {
		t.Fatalf("expected drop with no channel, got %d sends", fs.sendCount())
	}

	// Last-known channel fallback.
	b.SetChatID(7)
	b.SendNotification("via last channel")
	if got := fs.lastSend(t); got.channelID != "7" {
		t.Errorf("expected channel 7, got %q", got.channelID)
	}

	// Default channel takes priority.
	if err := idx.SetDefaultChat("a", platformName, 42); err != nil {
		t.Fatal(err)
	}
	b.SendNotification("via default")
	if got := fs.lastSend(t); got.channelID != "42" {
		t.Errorf("expected channel 42, got %q", got.channelID)
	}
}

// TestSendNotificationDirectBypassesBuffer verifies that SendNotificationDirect
// sends immediately even while a turn is active, returning the message ID.
func TestSendNotificationDirectBypassesBuffer(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	b.turnActive.Store(true)

	id := b.SendNotificationDirect("urgent")
	if fs.sendCount() != 1 {
		t.Fatalf("expected 1 immediate send, got %d", fs.sendCount())
	}
	if id == "" {
		t.Error("expected non-empty message ID")
	}
	if b.SendNotificationDirect("  ") != "" {
		t.Error("whitespace-only direct notification should return empty ID")
	}
}

// TestSendStartupNotification verifies the startup notice goes to the last
// known channel and is skipped silently when none is recorded.
func TestSendStartupNotification(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")

	b.SendStartupNotification("a")
	if fs.sendCount() != 0 {
		t.Fatalf("expected skip with no channel, got %d sends", fs.sendCount())
	}

	b.SetChatID(42)
	b.botUserID = "mybot"
	b.SendStartupNotification("a")
	got := fs.lastSend(t)
	if got.channelID != "42" {
		t.Errorf("expected channel 42, got %q", got.channelID)
	}
	if !strings.Contains(got.content, "mybot restarted") {
		t.Errorf("expected restart notice, got %q", got.content)
	}
}

// TestSendTextRouting verifies SendText errors without any channel, then
// routes via last-known and default channels as they become available.
func TestSendTextRouting(t *testing.T) {
	b, fs, idx := newTestBot(t, "a")

	if err := b.SendText("hello"); err == nil {
		t.Error("expected error with no channel configured")
	}

	b.SetChatID(7)
	if err := b.SendText("hello"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastSend(t); got.channelID != "7" {
		t.Errorf("expected last-known channel 7, got %q", got.channelID)
	}

	if err := idx.SetDefaultChat("a", platformName, 42); err != nil {
		t.Fatal(err)
	}
	if err := b.SendText("hello again"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastSend(t); got.channelID != "42" {
		t.Errorf("expected default channel 42, got %q", got.channelID)
	}
}

// TestSendTextToChatSkipsWhitespace verifies whitespace-only text is silently
// accepted but never hits the platform API.
func TestSendTextToChatSkipsWhitespace(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	if err := b.SendTextToChat(42, " \n "); err != nil {
		t.Fatal(err)
	}
	if fs.sendCount() != 0 {
		t.Errorf("expected no sends, got %d", fs.sendCount())
	}
}

// TestSendInjectedMessageHeader verifies the configured injected-message header
// is prepended for session and chat variants, and omitted when unset.
func TestSendInjectedMessageHeader(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.display.InjectedMessageHeader = "[system]"

	if err := b.SendInjectedMessage("a/c42/123", "wake up"); err != nil {
		t.Fatal(err)
	}
	got := fs.lastSend(t)
	if got.channelID != "42" {
		t.Errorf("expected chat ID from session key, got channel %q", got.channelID)
	}
	if !strings.HasPrefix(got.content, "[system]\nwake up") {
		t.Errorf("expected header prefix, got %q", got.content)
	}

	if err := b.SendInjectedToChat(7, "ping"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastSend(t); got.content != "[system]\nping" {
		t.Errorf("expected header on chat variant, got %q", got.content)
	}

	b.display.InjectedMessageHeader = ""
	if err := b.SendInjectedToChat(7, "bare"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastSend(t); got.content != "bare" {
		t.Errorf("expected no header, got %q", got.content)
	}
}

// TestSendToSessionFallback verifies SendToSession uses the default channel for
// keys without a chat ID and errors when neither is available.
func TestSendToSessionFallback(t *testing.T) {
	b, fs, idx := newTestBot(t, "a")

	if err := b.SendToSession("independent-key", "text"); err == nil {
		t.Error("expected error with no chat ID and no default")
	}

	if err := idx.SetDefaultChat("a", platformName, 42); err != nil {
		t.Fatal(err)
	}
	if err := b.SendToSession("independent-key", "text"); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastSend(t); got.channelID != "42" {
		t.Errorf("expected default channel 42, got %q", got.channelID)
	}
}

// TestSendMediaFile verifies sendMediaFile attaches the file with its base name
// and inline caption, and errors for missing files.
func TestSendMediaFile(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	path := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(path, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.SendPhotoToChat(42, path, "a caption"); err != nil {
		t.Fatal(err)
	}
	got := fs.lastSend(t)
	if got.content != "a caption" {
		t.Errorf("expected caption, got %q", got.content)
	}
	if len(got.fileNames) != 1 || got.fileNames[0] != "photo.png" {
		t.Errorf("expected attachment photo.png, got %v", got.fileNames)
	}
	if string(got.fileData[0]) != "png-bytes" {
		t.Error("attachment data mismatch")
	}

	if err := b.SendDocumentToChat(42, filepath.Join(t.TempDir(), "missing.pdf"), ""); err == nil {
		t.Error("expected error for missing file")
	}
}

// TestSendMediaToLastChannel verifies the caption and caption-less last-channel
// helpers error without a channel and deliver once one is known.
func TestSendMediaToLastChannel(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	path := filepath.Join(t.TempDir(), "clip.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.SendVoice(path); err == nil {
		t.Error("expected error with no last channel")
	}
	if err := b.SendAudio(path, "cap"); err == nil {
		t.Error("expected error with no last channel")
	}

	b.SetChatID(42)
	for _, send := range []func() error{
		func() error { return b.SendVoice(path) },
		func() error { return b.SendAudio(path, "cap") },
		func() error { return b.SendDocument(path, "cap") },
		func() error { return b.SendVideo(path, "cap") },
		func() error { return b.SendPhoto(path, "cap") },
		func() error { return b.SendAnimation(path, "cap") },
	} {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}
	if fs.sendCount() != 6 {
		t.Fatalf("expected 6 media sends, got %d", fs.sendCount())
	}
	if got := fs.lastSend(t); got.channelID != "42" {
		t.Errorf("expected channel 42, got %q", got.channelID)
	}
}

// TestSendVoiceData verifies audio bytes are sent as a voice.mp3 attachment to
// the last known channel, and error without one.
func TestSendVoiceData(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")

	if err := b.SendVoiceData([]byte("mp3")); err == nil {
		t.Error("expected error with no last channel")
	}

	b.SetChatID(42)
	if err := b.SendVoiceData([]byte("mp3")); err != nil {
		t.Fatal(err)
	}
	got := fs.lastSend(t)
	if len(got.fileNames) != 1 || got.fileNames[0] != "voice.mp3" {
		t.Errorf("expected voice.mp3 attachment, got %v", got.fileNames)
	}
	if string(got.fileData[0]) != "mp3" {
		t.Error("voice data mismatch")
	}
}

// TestSendTextWithButtons verifies button messages are sent to the default
// channel with built components and return the platform message ID.
func TestSendTextWithButtons(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	buttons := []platform.ButtonChoice{{Label: "Yes", Data: "yes"}}

	if _, err := b.SendTextWithButtons("pick", buttons, "cmd:"); err == nil {
		t.Error("expected error with no channel")
	}

	b.SetChatID(42)
	id, err := b.SendTextWithButtons("pick", buttons, "cmd:")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Error("expected message ID")
	}
	got := fs.lastSend(t)
	if got.content != "pick" || len(got.components) != 1 {
		t.Errorf("expected text with 1 action row, got %q with %d rows", got.content, len(got.components))
	}

	fs.sendErr = errors.New("boom")
	if _, err := b.SendTextWithButtons("pick", buttons, "cmd:"); err == nil {
		t.Error("expected send error to propagate")
	}
}

// TestEditMessageText verifies edits replace content and strip buttons, and
// error without a channel.
func TestEditMessageText(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")

	if err := b.EditMessageText("9", "new"); err == nil {
		t.Error("expected error with no channel")
	}

	b.SetChatID(42)
	if err := b.EditMessageText("9", "new"); err != nil {
		t.Fatal(err)
	}
	got := fs.lastEdit(t)
	if got.msgID != "9" || got.content != "new" {
		t.Errorf("unexpected edit %+v", got)
	}
	if got.components == nil || len(got.components) != 0 {
		t.Errorf("expected buttons stripped (empty non-nil components), got %v", got.components)
	}
}

// TestEditMessageWithButtons verifies edits can replace both text and buttons.
func TestEditMessageWithButtons(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	buttons := []platform.ButtonChoice{{Label: "Go", Data: "go"}}

	if err := b.EditMessageWithButtons("9", "new", buttons, "cmd:"); err == nil {
		t.Error("expected error with no channel")
	}

	b.SetChatID(42)
	if err := b.EditMessageWithButtons("9", "new", buttons, "cmd:"); err != nil {
		t.Fatal(err)
	}
	got := fs.lastEdit(t)
	if got.content != "new" || len(got.components) != 1 {
		t.Errorf("expected new text with 1 action row, got %+v", got)
	}
}

// TestSetTyping verifies the typing indicator lifecycle: no-op without a
// channel, one immediate typing call with a cancellable ticker when started,
// idempotent restart, and cancellation on stop.
func TestSetTyping(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")

	// No channel known: nothing happens.
	b.SetTyping(true)
	if fs.typingCalls != 0 || b.typingCancel != nil {
		t.Fatal("expected no typing without a channel")
	}

	b.SetChatID(42)
	b.SetTyping(true)
	if fs.typingCalls != 1 {
		t.Fatalf("expected 1 immediate typing call, got %d", fs.typingCalls)
	}
	if b.typingCancel == nil {
		t.Fatal("expected typing ticker running")
	}

	// Starting again while running is a no-op.
	b.SetTyping(true)
	if fs.typingCalls != 1 {
		t.Errorf("expected restart no-op, got %d typing calls", fs.typingCalls)
	}

	b.SetTyping(false)
	if b.typingCancel != nil {
		t.Error("expected ticker cancelled")
	}
	// Stopping again is safe.
	b.SetTyping(false)
}

// TestSendReply verifies sendReply routes the response to the originating
// message's channel.
func TestSendReply(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.sendReply(testDiscordMessage("55", "u1", "hi"), "the answer")
	got := fs.lastSend(t)
	if got.channelID != "55" || got.content != "the answer" {
		t.Errorf("unexpected reply %+v", got)
	}
}

// TestSendNotificationImmediateChunksLongText verifies that an over-length
// notification (e.g. startup proactive-warnings) is split into multiple sends
// within Discord's 2000-char cap rather than sent raw and rejected with HTTP
// 400 (#810). The returned anchor ID is the first chunk's message ID.
func TestSendNotificationImmediateChunksLongText(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	long := strings.Repeat("warning line\n", 400) // ~5200 chars

	id := b.SendNotificationDirect(long)

	if fs.sendCount() < 3 {
		t.Fatalf("expected at least 3 chunked sends, got %d", fs.sendCount())
	}
	var rebuilt strings.Builder
	for _, s := range fs.sends {
		if len(s.content) > discordMaxChars {
			t.Errorf("notification chunk exceeds limit: %d chars", len(s.content))
		}
		if s.channelID != "42" {
			t.Errorf("chunk sent to wrong channel %q", s.channelID)
		}
		rebuilt.WriteString(s.content)
	}
	if rebuilt.String() != long {
		t.Error("concatenated notification chunks do not reproduce original text")
	}
	if id != "1" {
		t.Errorf("expected first chunk's message ID %q, got %q", "1", id)
	}
}

// TestSendNotificationImmediateShortReturnsID verifies the common single-chunk
// path is unchanged: one send, and the message ID is returned.
func TestSendNotificationImmediateShortReturnsID(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)

	id := b.SendNotificationDirect("short notice")

	if fs.sendCount() != 1 {
		t.Fatalf("expected exactly 1 send, got %d", fs.sendCount())
	}
	if id != "1" {
		t.Errorf("expected message ID %q, got %q", "1", id)
	}
}
