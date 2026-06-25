package app

import (
	"context"
	"errors"
	"sync"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/platform"
)

// errMediaUnsupported is returned by the media-send methods. Media is a later
// slice (docs/02 §11.4); until then the app is text + streaming only.
var errMediaUnsupported = errors.New("app: media send not yet supported")

// appConn is the per-agent platform.Connection for the app provider. One
// instance per agent; it routes session-scoped sends to the right physical
// socket via the hub's binding map, and acts as the agent.Driver that builds
// the per-turn streaming sink.
//
// Slice 1 intentionally does NOT implement platform.ButtonSender: without it,
// foci's ask/permission/plan machinery falls back to a text prompt that the
// app shows as a normal message and the user answers by typing — which routes
// back through the inbound message path. Native buttons + callback routing are
// slice 2.
type appConn struct {
	hub      *Hub
	agentID  string
	agentRef *agent.Agent

	mu             sync.Mutex
	defaultSession string
	lastChatID     int64
}

var (
	_ platform.Connection = (*appConn)(nil)
	_ agent.Driver        = (*appConn)(nil)
)

// --- identity ---

func (c *appConn) PlatformName() string { return "app" }
func (c *appConn) Username() string     { return c.agentID }

// --- session management ---

func (c *appConn) SessionKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.defaultSession
}

func (c *appConn) DefaultSessionKey() string         { return c.SessionKey() }
func (c *appConn) DefaultSessionKeyOrEmpty() string  { return c.SessionKey() }
func (c *appConn) SetSessionKey(key string)          { c.setDefault(key) }
func (c *appConn) SetSessionKeyDirect(key string)    { c.setDefault(key) }

func (c *appConn) setDefault(key string) {
	c.mu.Lock()
	c.defaultSession = key
	c.mu.Unlock()
}

func (c *appConn) SetChatID(chatID int64) {
	c.mu.Lock()
	c.lastChatID = chatID
	c.mu.Unlock()
}

func (c *appConn) ChatID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastChatID
}

func (c *appConn) SessionKeyForChat(chatID int64) string {
	return c.hub.sessionKeyForChat(c.agentID, chatID)
}

// UpdateChatSessionKey persists a rotated session key (compaction/reset) and
// re-points the live binding, so the app's conversation thread survives the
// rotation (the conversationId stays stable; only the underlying session key
// changes — nicer than Telegram where the chat IS the identity).
func (c *appConn) UpdateChatSessionKey(chatID int64, newKey string) {
	if idx := c.hub.deps.SessionIndex; idx != nil {
		_ = idx.SetChatMetadata(c.agentID, "app", chatID, "session", newKey)
	}
	var rotated []*convBinding
	c.hub.mu.Lock()
	for old, b := range c.hub.bySession {
		if b.chatID == chatID && b.agentID == c.agentID {
			delete(c.hub.bySession, old)
			b.sessionKey = newKey
			c.hub.bySession[newKey] = b
			b.client.mu.Lock()
			b.client.convByID[b.convID] = b
			b.client.mu.Unlock()
			rotated = append(rotated, b)
		}
	}
	c.hub.mu.Unlock()

	// Tell the app its conversation's session key rotated; the conversationId
	// stays stable so the thread identity survives the rotation.
	for _, b := range rotated {
		b.send(fap.SessionUpdate{ConversationID: b.convID, SessionKey: newKey, Reason: "rotated"})
	}
}

// --- messaging ---

func (c *appConn) SendToSession(sessionKey, text string) error {
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if clean == "" {
		return nil
	}
	b := c.hub.bindingForSession(sessionKey)
	if b == nil {
		return nil // offline: buffering/push is a later slice
	}
	b.send(fap.ServerMessage{
		ConversationID: b.convID,
		MessageID:      fap.NewULID(),
		Role:           "agent",
		Text:           clean,
	})
	return nil
}

func (c *appConn) SendInjectedMessage(sessionKey, text string) error {
	b := c.hub.bindingForSession(sessionKey)
	if b == nil {
		return nil
	}
	b.send(fap.ServerMessage{
		ConversationID: b.convID,
		MessageID:      fap.NewULID(),
		Role:           "system",
		Text:           text,
	})
	return nil
}

func (c *appConn) SendText(text string) error {
	return c.SendToSession(c.SessionKey(), text)
}

func (c *appConn) SendTextToChat(chatID int64, text string) error {
	return c.SendToSession(c.SessionKeyForChat(chatID), text)
}

func (c *appConn) SendNotification(text string) {
	c.deliverNotification(c.SessionKey(), text)
}

func (c *appConn) SendNotificationDirect(text string) string {
	c.deliverNotification(c.SessionKey(), text)
	return ""
}

func (c *appConn) deliverNotification(sessionKey, text string) {
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if clean == "" {
		return
	}
	b := c.hub.bindingForSession(sessionKey)
	if b == nil {
		return
	}
	b.send(fap.Notification{ConversationID: b.convID, Text: clean, Level: "info"})
}

func (c *appConn) SetTyping(typing bool) {
	if b := c.hub.bindingForSession(c.SessionKey()); b != nil {
		b.send(fap.Typing{ConversationID: b.convID, On: typing})
	}
}

// --- media (later slice) ---

func (c *appConn) SendDocument(string, string) error  { return errMediaUnsupported }
func (c *appConn) SendVoice(string) error             { return errMediaUnsupported }
func (c *appConn) SendVideo(string, string) error     { return errMediaUnsupported }
func (c *appConn) SendPhoto(string, string) error     { return errMediaUnsupported }
func (c *appConn) SendAudio(string, string) error     { return errMediaUnsupported }
func (c *appConn) SendAnimation(string, string) error { return errMediaUnsupported }
func (c *appConn) SendVoiceData([]byte) error         { return errMediaUnsupported }

func (c *appConn) SendDocumentToChat(int64, string, string) error  { return errMediaUnsupported }
func (c *appConn) SendVoiceToChat(int64, string) error             { return errMediaUnsupported }
func (c *appConn) SendVideoToChat(int64, string, string) error     { return errMediaUnsupported }
func (c *appConn) SendPhotoToChat(int64, string, string) error     { return errMediaUnsupported }
func (c *appConn) SendAudioToChat(int64, string, string) error     { return errMediaUnsupported }
func (c *appConn) SendAnimationToChat(int64, string, string) error { return errMediaUnsupported }
func (c *appConn) SendVoiceDataToChat(int64, []byte) error         { return errMediaUnsupported }

// --- agent.Driver ---

// WrapTurn runs the turn. Typing lifecycle is handled by the streaming sink
// (TurnStart/TurnComplete), so no extra wrapping is needed for slice 1.
func (c *appConn) WrapTurn(_ context.Context, fn func() error) error {
	return fn()
}

// NewTurnSink builds the per-turn streaming sink bound to the envelope's
// conversation. Returns a nil sink if no live binding exists for the session
// (the socket dropped) — the agent skips rendering in that case.
func (c *appConn) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	b := c.hub.bindingForSession(env.SessionKey)
	if b == nil {
		return nil, nil
	}
	c.mu.Lock()
	c.defaultSession = env.SessionKey
	c.lastChatID = env.ChatID
	c.mu.Unlock()
	return newAppSink(b), func() {}
}

func (c *appConn) Connection() platform.Connection { return c }
