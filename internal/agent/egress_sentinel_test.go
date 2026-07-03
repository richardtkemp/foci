package agent

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/workspace"
)

// recordingConn is a platform.Connection that records the text handed to
// SendToSession (the method agent replies are delivered through) and is a
// no-op for everything else. It lets a delivery sink run its real
// platform.IsSilent gate against a captured send, so a test can assert on
// exactly what would reach the user's chat.
type recordingConn struct {
	sent        []string
	typingCalls []bool
}

func (c *recordingConn) SendToSession(_ string, text string) error {
	c.sent = append(c.sent, text)
	return nil
}
func (c *recordingConn) SetTyping(on bool) { c.typingCalls = append(c.typingCalls, on) }

// Remaining Connection surface — unused by these tests. They panic so a future
// coupling change fails loudly rather than silently passing through an
// unexercised path.
func (c *recordingConn) SendText(string) error              { panic("SendText") }
func (c *recordingConn) SendTextToChat(int64, string) error { panic("SendTextToChat") }
func (c *recordingConn) SessionKey() string                 { return "" }
func (c *recordingConn) SendDocument(string, string) error  { panic("SendDocument") }
func (c *recordingConn) SendVoice(string) error             { panic("SendVoice") }
func (c *recordingConn) SendVideo(string, string) error     { panic("SendVideo") }
func (c *recordingConn) SendPhoto(string, string) error     { panic("SendPhoto") }
func (c *recordingConn) SendAudio(string, string) error     { panic("SendAudio") }
func (c *recordingConn) SendAnimation(string, string) error { panic("SendAnimation") }
func (c *recordingConn) SendVoiceData([]byte) error         { panic("SendVoiceData") }
func (c *recordingConn) SendDocumentToChat(int64, string, string) error {
	panic("SendDocumentToChat")
}
func (c *recordingConn) SendVoiceToChat(int64, string) error         { panic("SendVoiceToChat") }
func (c *recordingConn) SendVideoToChat(int64, string, string) error { panic("SendVideoToChat") }
func (c *recordingConn) SendPhotoToChat(int64, string, string) error { panic("SendPhotoToChat") }
func (c *recordingConn) SendAudioToChat(int64, string, string) error { panic("SendAudioToChat") }
func (c *recordingConn) SendAnimationToChat(int64, string, string) error {
	panic("SendAnimationToChat")
}
func (c *recordingConn) SendVoiceDataToChat(int64, []byte) error  { panic("SendVoiceDataToChat") }
func (c *recordingConn) PlatformName() string                     { return "test" }
func (c *recordingConn) SessionKeyForChat(int64) string           { return "" }
func (c *recordingConn) DefaultSessionKey() string                { return "" }
func (c *recordingConn) SetSessionKey(string)                     {}
func (c *recordingConn) SetSessionKeyDirect(string)               {}
func (c *recordingConn) SetChatID(int64)                          {}
func (c *recordingConn) ChatID() int64                            { return 0 }
func (c *recordingConn) Username() string                         { return "" }
func (c *recordingConn) SendInjectedMessage(string, string) error { panic("SendInjectedMessage") }
func (c *recordingConn) SendNotification(string)                  {}
func (c *recordingConn) SendNotificationDirect(string) string     { return "" }

// newSentinelTestAgent builds a minimal API-backend agent (DelegatedManager is
// nil, so HandleMessage selects APITransport) whose single inference call
// returns reply. The agent loads no real config beyond what a turn needs.
func newSentinelTestAgent(t *testing.T, reply string) *Agent {
	t.Helper()
	client := newTestClient(func(_ *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_sentinel",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent(reply),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	return &Agent{
		Client:    client,
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "claude-haiku-4-5",
	}
}

// TestAPIEgress_TrailingSentinelStripped is the API-backend (APITransport)
// counterpart to the cc-stub L2 egress tests for the delegated backend. It
// proves the end-to-end trailing-sentinel behaviour for a model reply that
// appends [[NO_RESPONSE]] to real text: the agent runs a real inference turn,
// the FinalText flows through a real delivery sink (SessionSink, whose gate is
// platform.IsSilent), and the text handed to the connection must contain the
// real reply WITHOUT the trailing sentinel literal.
//
// Pre-fix this FAILS — IsSilent is exact-match, so "real reply.[[NO_RESPONSE]]"
// is not equal to the sentinel, the gate does not fire, and the raw text
// (sentinel included) is delivered. Post-fix the chokepoint strips the trailing
// sentinel and delivers the clean text.
func TestAPIEgress_TrailingSentinelStripped(t *testing.T) {
	const realReply = "all clean, nothing uncommitted"
	ag := newSentinelTestAgent(t, realReply+"\n[[NO_RESPONSE]]")

	sk := "test/iresp"
	conn := &recordingConn{}
	sink := turn.NewSessionSink(conn, sk, "test")
	ctx := turnevent.WithSink(context.Background(), sink)

	if err := ag.HandleMessage(ctx, sk, []string{"status?"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if len(conn.sent) != 1 {
		t.Fatalf("expected exactly one delivered message, got %d: %q", len(conn.sent), conn.sent)
	}
	got := conn.sent[0]
	if !strings.Contains(got, realReply) {
		t.Errorf("delivered message lost the real reply:\n  %q\nexpected it to contain %q", got, realReply)
	}
	if strings.Contains(got, "[[NO_RESPONSE]]") {
		t.Errorf("delivered message leaked the trailing sentinel:\n  %q\nexpected [[NO_RESPONSE]] to be stripped", got)
	}
}

// TestAPIEgress_PureSentinelSuppressed pins the unchanged behaviour: a reply
// that is *only* the sentinel must be fully suppressed — nothing delivered.
// This guards against a fix that over-strips into delivering an empty message.
func TestAPIEgress_PureSentinelSuppressed(t *testing.T) {
	ag := newSentinelTestAgent(t, "[[NO_RESPONSE]]")

	sk := "test/iresp"
	conn := &recordingConn{}
	sink := turn.NewSessionSink(conn, sk, "test")
	ctx := turnevent.WithSink(context.Background(), sink)

	if err := ag.HandleMessage(ctx, sk, []string{"anything?"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if len(conn.sent) != 0 {
		t.Fatalf("pure-sentinel reply should deliver nothing, got %d message(s): %q", len(conn.sent), conn.sent)
	}
}

// compile-time assertion that recordingConn satisfies the full interface.
var _ platform.Connection = (*recordingConn)(nil)
