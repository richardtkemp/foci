package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/session"
	"foci/internal/tooldetail"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestSendText_RoutesToLastChatWithFallbackError(t *testing.T) {
	// Proves SendText errors with no chat available, then delivers to the
	// last known chat once one exists.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	if err := b.SendText("hi"); err == nil || !strings.Contains(err.Error(), "no chat ID") {
		t.Errorf("err = %v, want no-chat error", err)
	}
	b.SetChatID(12345)
	if err := b.SendText("hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1", mock.sentCount())
	}
}

func TestSendInjectedMessage_HeaderAndSessionRouting(t *testing.T) {
	// Proves injected messages are prefixed with the configured header and
	// routed to the chat embedded in the session key.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.display.InjectedMessageHeader = "[injected]"

	chatKey := session.NewChatSessionKey("scout", 777)
	if err := b.SendInjectedMessage(chatKey, "wake"); err != nil {
		t.Fatalf("SendInjectedMessage: %v", err)
	}
	if !strings.Contains(mock.lastSendInjected, "[injected]") || !strings.Contains(mock.lastSendInjected, "wake") {
		t.Errorf("sent = %q, want header + text", mock.lastSendInjected)
	}

	// Session key without a chat ID and no default chat: error.
	if err := b.SendInjectedMessage("agent:scout:main", "x"); err == nil {
		t.Error("expected error for chatless session with no default")
	}
}

func TestSendNotificationDirect(t *testing.T) {
	// Proves direct notifications bypass the turn buffer (delivered even
	// mid-turn), return the message ID, and skip empty text.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)
	b.turnActive.Store(true)

	if id := b.SendNotificationDirect("urgent"); id != "1" {
		t.Errorf("id = %q, want 1", id)
	}
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1 despite active turn", mock.sentCount())
	}
	if id := b.SendNotificationDirect("  "); id != "" {
		t.Errorf("empty notification id = %q, want \"\"", id)
	}
}

func TestSendNotificationImmediate_Failures(t *testing.T) {
	// Proves notification delivery returns "" both when no chat is known and
	// when the API send fails.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	if id := b.sendNotificationImmediate("hello"); id != "" {
		t.Errorf("id = %q, want \"\" without chat", id)
	}
	b.SetChatID(12345)
	mock.sendErr = fmt.Errorf("boom")
	if id := b.sendNotificationImmediate("hello"); id != "" {
		t.Errorf("id = %q, want \"\" on API failure", id)
	}
}

func TestSessionKeyForChatID(t *testing.T) {
	// Proves session key resolution for steer routing: secondary bots use
	// their override, agent-bound primaries derive per-chat keys, and
	// unbound bots fall back to their own key.
	b, _ := testBot(nil, command.NewRegistry())
	b.isSecondary = true
	b.SetSessionKeyDirect("agent:scout:branch:y")
	if got := b.SessionKeyForChatID(7); got != "agent:scout:branch:y" {
		t.Errorf("secondary = %q, want override", got)
	}

	b2, _ := testBot(nil, command.NewRegistry())
	b2.agentID = "scout"
	b2.chatmeta.AgentID = "scout"
	b2.SetSessionKeyDirect("")
	if got := b2.SessionKeyForChatID(7); got != b2.SessionKeyForChat(7) {
		t.Errorf("primary = %q, want per-chat key", got)
	}

	b3, _ := testBot(nil, command.NewRegistry())
	if got := b3.SessionKeyForChatID(7); got != "agent:test:main" {
		t.Errorf("unbound = %q, want bot key", got)
	}
}

func TestSetToolDetailStore_RestoresEntries(t *testing.T) {
	// Proves SetToolDetailStore loads persisted tool details into the
	// in-memory expansion map so inline keyboards survive restarts.
	dbPath := filepath.Join(t.TempDir(), "d.db")
	store, err := tooldetail.NewStore(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	store.Store(100, "compact", "full", "result")

	b, _ := testBot(nil, command.NewRegistry())
	b.SetToolDetailStore(store)
	defer func() { _ = store.Close() }()

	entry, ok := b.toolStore.Load("100")
	if !ok {
		t.Fatal("entry not restored")
	}
	if entry.CompactText != "compact" || entry.Result != "result" {
		t.Errorf("restored entry = %+v", entry)
	}

	// Nil store: safe no-op.
	b2, _ := testBot(nil, command.NewRegistry())
	b2.SetToolDetailStore(nil)
}

func TestSetCommandContext_SecondaryReleaseFunc(t *testing.T) {
	// Proves SetCommandContext on a secondary bot installs a working
	// ReleaseFunc in the dispatcher's command context: invoking it from a
	// command returns the bot to its pool by clearing the session key.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "done",
		Execute: func(_ context.Context, _ command.Request, cc command.CommandContext) (command.Response, error) {
			if !cc.IsSecondaryBot {
				t.Error("IsSecondaryBot should be true in dispatched context")
			}
			if cc.ReleaseFunc == nil {
				t.Fatal("ReleaseFunc not installed")
			}
			cc.ReleaseFunc()
			return command.Response{Text: "released"}, nil
		},
	})

	pool := NewPool()
	b, _ := testBot([]string{"111"}, cmds)
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 1, Username: "facetbot"}}
	b.SetSecondary(pool)
	pool.Add(b)
	b.SetSessionKeyDirect("agent:scout:branch:z")

	b.SetCommandContext(command.CommandContext{})
	if b.dispatcher == nil {
		t.Fatal("dispatcher not installed")
	}

	outcome := b.dispatcher.DispatchCommand(context.Background(), "/done", 12345, "111")
	if outcome.NotHandled {
		t.Fatal("/done not handled")
	}
	if b.SessionKey() != "" {
		t.Errorf("session key = %q, want cleared after release", b.SessionKey())
	}
}

func TestBuildReceivedMessage_QuoteAndReplyContext(t *testing.T) {
	// Proves quoted text takes priority over the full replied-to message and
	// reply context falls back to the replied message's text or caption.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "my reply")
	msg.Quote = &gotgbot.TextQuote{Text: "highlighted bit"}
	msg.ReplyToMessage = &gotgbot.Message{Text: "whole original"}
	qm, ok := b.buildReceivedMessage(context.Background(), msg)
	if !ok {
		t.Fatal("message dropped")
	}
	// The prefix carries the replied-to message's send time, so match on the
	// marker + payload rather than an exact string (timestamp is TZ-dependent).
	if !strings.Contains(qm.text, "[Quoting (") || !strings.Contains(qm.text, "highlighted bit]") || strings.Contains(qm.text, "whole original") {
		t.Errorf("text = %q, want quote preferred over reply", qm.text)
	}

	msg2 := makeMsg(111, "owner", "my reply")
	msg2.ReplyToMessage = &gotgbot.Message{Caption: "photo caption"}
	qm2, _ := b.buildReceivedMessage(context.Background(), msg2)
	if !strings.Contains(qm2.text, "[Replying to (") || !strings.Contains(qm2.text, "photo caption]") {
		t.Errorf("text = %q, want reply caption context", qm2.text)
	}
}

func TestBuildReceivedMessage_VoiceWithoutSTT(t *testing.T) {
	// Proves a voice note without an STT provider is dropped with a helpful
	// reply instead of being silently swallowed.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "owner", "")
	msg.Voice = &gotgbot.Voice{FileId: "v1"}

	_, ok := b.buildReceivedMessage(context.Background(), msg)
	if ok {
		t.Error("voice message should be dropped without STT")
	}
	if mock.sentCount() != 1 || !strings.Contains(mock.lastSendInjected, "STT provider") {
		t.Errorf("reply = %q, want STT hint", mock.lastSendInjected)
	}
}

func TestDownloadAttachment_ViaStubServer(t *testing.T) {
	// Proves downloadAttachment fetches the file through GetFile + the file
	// URL (against a local stub), saves it to the received-files dir, and
	// reports failure for unknown file IDs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/photos/p1.jpg") {
			_, _ = w.Write([]byte("jpeg-bytes"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.apiBase = srv.URL
	b.botToken = "tok"
	b.display.ReceivedFilesDir = t.TempDir()
	mock.files = map[string]string{"p1": "photos/p1.jpg"}

	att, ok := b.downloadAttachment("p1", "image/jpeg", 12345, "")
	if !ok {
		t.Fatal("downloadAttachment failed")
	}
	if string(att.data) != "jpeg-bytes" || att.mediaType != "image/jpeg" {
		t.Errorf("attachment = %q/%q", att.data, att.mediaType)
	}
	if att.savedPath == "" || !strings.HasSuffix(att.savedPath, ".jpg") {
		t.Errorf("savedPath = %q, want .jpg file", att.savedPath)
	}

	// Unknown file ID: GetFile fails, user gets a retry hint.
	if _, ok := b.downloadAttachment("nope", "image/jpeg", 12345, ""); ok {
		t.Error("expected failure for unknown file ID")
	}
}

func TestDownloadFile_404FailsWithoutRetry(t *testing.T) {
	// Proves a 4xx download status fails immediately (no retry loop) with
	// the status in the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.apiBase = srv.URL
	b.botToken = "tok"
	mock.files = map[string]string{"f1": "files/f1.bin"}

	_, err := b.downloadFile("f1")
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Errorf("err = %v, want status 404", err)
	}
}

func TestSendNotificationImmediate_ChunksLongText(t *testing.T) {
	// Proves an over-length notification (e.g. startup proactive-warnings) is
	// split into multiple sends within Telegram's 4096-char cap rather than sent
	// raw and rejected (#810). The returned anchor ID is the first chunk's ID.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)
	long := strings.Repeat("warning line\n", 500) // ~6500 chars > 4096

	id := b.SendNotificationDirect(long)

	if mock.sentCount() < 2 {
		t.Fatalf("expected chunked sends (>=2), got %d", mock.sentCount())
	}
	if id != "1" {
		t.Errorf("anchor id = %q, want first chunk's id 1", id)
	}
}

func TestSendNotificationImmediate_ShortSingleSend(t *testing.T) {
	// Proves the common single-chunk path is unchanged: one send, ID returned.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	id := b.SendNotificationDirect("short notice")

	if mock.sentCount() != 1 {
		t.Fatalf("expected exactly 1 send, got %d", mock.sentCount())
	}
	if id != "1" {
		t.Errorf("id = %q, want 1", id)
	}
}
