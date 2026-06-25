package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/voice"
)

// defaultPromptTTL is the advisory expiry advertised on an interactive prompt
// (wire §8). Foci core owns real expiry; this only tells the app's UI when to
// grey out stale buttons.
const defaultPromptTTL = 24 * time.Hour

// appConn is the per-agent platform.Connection for the app provider. One
// instance per agent; it routes session-scoped sends to the right physical
// socket via the hub's binding map, and acts as the agent.Driver that builds
// the per-turn streaming sink.
//
// It implements platform.ButtonSender (slice 2): foci's permission / ask / plan
// machinery renders native buttons via `interactive` frames and routes taps back
// through platform.HandleInteractiveCallback — see dispatch.go and the
// SendTextWithButtons/EditMessage* methods below.
// agentCore is the slice of *agent.Agent the app provider drives. Narrowing it
// to an interface keeps the inbound dispatch path (routeUserTurn) and the meta
// status chips testable with a fake, without constructing a full Agent.
type agentCore interface {
	Enqueue(agent.Envelope)
	MetaStatus(sessionKey string) (manaPct *int, manaState, gap string)
}

type appConn struct {
	hub      *Hub
	agentID  string
	agentRef agentCore
	commands *command.Registry
	cmdCtx   command.CommandContext
	stt      voice.STT // inbound voice transcription; nil = unsupported

	mu             sync.Mutex
	defaultSession string
	lastChatID     int64
}

var (
	_ platform.Connection        = (*appConn)(nil)
	_ platform.ButtonSender      = (*appConn)(nil)
	_ platform.BatchButtonSender = (*appConn)(nil)
	_ agent.Driver               = (*appConn)(nil)
)

// errNoBinding is returned by ButtonSender sends when the prompt's conversation
// has no live socket (the device dropped mid-turn). Offline buffering + push is
// a later slice; until then foci treats the send as failed and the prompt's
// 24h expiry (onExpire → deny) eventually unblocks the turn.
var errNoBinding = errors.New("app: no live socket for conversation")

// --- identity ---

func (c *appConn) PlatformName() string { return "app" }
func (c *appConn) Username() string     { return c.agentID }

// --- session management ---

func (c *appConn) SessionKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.defaultSession
}

func (c *appConn) DefaultSessionKey() string        { return c.SessionKey() }
func (c *appConn) DefaultSessionKeyOrEmpty() string { return c.SessionKey() }
func (c *appConn) SetSessionKey(key string)         { c.setDefault(key) }
func (c *appConn) SetSessionKeyDirect(key string)   { c.setDefault(key) }

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
			b.mu.Lock()
			b.sessionKey = newKey
			b.mu.Unlock()
			c.hub.bySession[newKey] = b
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

// --- media (slice 4) ---
//
// Each Sender method stores the payload as a blob (decoupling it from the
// caller's temp file) and emits a `media` frame referencing it; the app fetches
// the bytes out-of-band via GET /app/blob/<id>. Offline (no live socket) the
// media frame buffers like any other and replays on reconnect — the blob
// survives independently under its own TTL.

func (c *appConn) SendDocument(path, caption string) error {
	return c.sendMediaFile(c.SessionKey(), path, caption, fap.MediaDocument)
}
func (c *appConn) SendVoice(path string) error {
	return c.sendMediaFile(c.SessionKey(), path, "", fap.MediaVoice)
}
func (c *appConn) SendVideo(path, caption string) error {
	return c.sendMediaFile(c.SessionKey(), path, caption, fap.MediaVideo)
}
func (c *appConn) SendPhoto(path, caption string) error {
	return c.sendMediaFile(c.SessionKey(), path, caption, fap.MediaPhoto)
}
func (c *appConn) SendAudio(path, caption string) error {
	return c.sendMediaFile(c.SessionKey(), path, caption, fap.MediaAudio)
}
func (c *appConn) SendAnimation(path, caption string) error {
	return c.sendMediaFile(c.SessionKey(), path, caption, fap.MediaAnimation)
}
func (c *appConn) SendVoiceData(audioData []byte) error {
	return c.sendMediaBytes(c.SessionKey(), audioData, fap.MediaVoice, "voice.mp3", "audio/mpeg")
}

func (c *appConn) SendDocumentToChat(chatID int64, path, caption string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, caption, fap.MediaDocument)
}
func (c *appConn) SendVoiceToChat(chatID int64, path string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, "", fap.MediaVoice)
}
func (c *appConn) SendVideoToChat(chatID int64, path, caption string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, caption, fap.MediaVideo)
}
func (c *appConn) SendPhotoToChat(chatID int64, path, caption string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, caption, fap.MediaPhoto)
}
func (c *appConn) SendAudioToChat(chatID int64, path, caption string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, caption, fap.MediaAudio)
}
func (c *appConn) SendAnimationToChat(chatID int64, path, caption string) error {
	return c.sendMediaFile(c.SessionKeyForChat(chatID), path, caption, fap.MediaAnimation)
}
func (c *appConn) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	return c.sendMediaBytes(c.SessionKeyForChat(chatID), audioData, fap.MediaVoice, "voice.mp3", "audio/mpeg")
}

func (c *appConn) sendMediaFile(sessionKey, path, caption, kind string) error {
	b := c.hub.bindingForSession(sessionKey)
	if b == nil {
		return nil
	}
	meta, err := c.hub.blobs.putFile(path, kind)
	if err != nil {
		return err
	}
	c.emitMedia(b, meta, caption)
	return nil
}

func (c *appConn) sendMediaBytes(sessionKey string, data []byte, kind, name, mimeType string) error {
	b := c.hub.bindingForSession(sessionKey)
	if b == nil {
		return nil
	}
	meta, err := c.hub.blobs.putBytes(data, kind, name, mimeType)
	if err != nil {
		return err
	}
	c.emitMedia(b, meta, "")
	return nil
}

func (c *appConn) emitMedia(b *convBinding, meta *blobMeta, caption string) {
	b.send(fap.Media{
		ConversationID: b.convID,
		MessageID:      fap.NewULID(),
		BlobID:         meta.id,
		Kind:           meta.kind,
		MIME:           meta.mime,
		Name:           meta.name,
		Caption:        caption,
		Size:           meta.size,
	})
}

// --- platform.ButtonSender (slice 2: interactive prompts) ---

// SendTextWithButtons emits an `interactive` frame for the in-flight turn's
// conversation. foci pre-encodes each button's Data as "<promptID>:<index>"
// (platform.SendInteractiveMessageWithID), so we carry it verbatim and the app
// echoes it back in InteractiveResponse.data for routing — no app-side button
// registry needed. The returned msgID is the promptID, which proactive edits
// (cancel/expiry) pass to EditMessageText to address the prompt.
func (c *appConn) SendTextWithButtons(text string, buttons []platform.ButtonChoice, _ string) (string, error) {
	b := c.hub.bindingForSession(c.SessionKey())
	if b == nil {
		return "", errNoBinding
	}
	promptID := promptIDFromButtons(buttons)
	c.hub.registerPrompt(promptID, b)
	b.send(fap.Interactive{
		ConversationID: b.convID,
		PromptID:       promptID,
		Text:           text,
		Choices:        toChoices(buttons),
		ExpiresAt:      time.Now().Add(defaultPromptTTL).Format(time.RFC3339),
	})
	return promptID, nil
}

// SendInteractiveBatch emits ONE `interactive` frame carrying every question of
// a multi-question ask, for clients that advertised the "interactiveBatch"
// capability. The app renders a single form and replies with one
// InteractiveResponse.Answers (all answers, positional), routed back via the
// hub's batched-prompt registry. Returns batched=false when it can't batch — no
// live socket, or the client didn't advertise the capability — so the ask layer
// falls back to the sequential SendTextWithButtons path. Implements
// platform.BatchButtonSender.
func (c *appConn) SendInteractiveBatch(promptID string, questions []platform.BatchQuestion, onResponse func(answers []string)) (bool, error) {
	b := c.hub.bindingForSession(c.SessionKey())
	if b == nil {
		return false, nil // offline → caller uses the sequential path
	}
	if !b.clientHasFeature(featureInteractiveBatch) {
		return false, nil // legacy/uncapable client → sequential one-at-a-time
	}
	c.hub.registerBatchPrompt(promptID, b, onResponse)
	qs := make([]fap.Question, len(questions))
	for i, q := range questions {
		qs[i] = fap.Question{Text: q.Text, Choices: toChoices(q.Choices)}
	}
	b.send(fap.Interactive{
		ConversationID: b.convID,
		PromptID:       promptID,
		Questions:      qs,
		ExpiresAt:      time.Now().Add(defaultPromptTTL).Format(time.RFC3339),
	})
	return true, nil
}

// EditMessageText replaces a prompt's text and removes its buttons. Terminal:
// the resolution / cancel / expiry edit. msgID is the promptID returned by
// SendTextWithButtons. Idempotent — an unknown promptID (already resolved) is a
// no-op.
func (c *appConn) EditMessageText(msgID, text string) error {
	b := c.hub.bindingForPrompt(msgID)
	if b == nil {
		return nil
	}
	c.hub.deletePrompt(msgID)
	b.send(fap.InteractiveEdit{ConversationID: b.convID, PromptID: msgID, Text: text})
	return nil
}

// EditMessageWithButtons replaces a prompt's text and buttons (non-terminal).
// foci core never calls this today — multi-question asks re-present via a fresh
// SendInteractiveMessageWithID (same promptID, last-writer-wins) rather than an
// edit — but the ButtonSender contract requires it, so it mirrors the send path.
func (c *appConn) EditMessageWithButtons(msgID, text string, buttons []platform.ButtonChoice, _ string) error {
	b := c.hub.bindingForPrompt(msgID)
	if b == nil {
		return nil
	}
	b.send(fap.InteractiveEdit{
		ConversationID: b.convID,
		PromptID:       msgID,
		Text:           text,
		Choices:        toChoices(buttons),
	})
	return nil
}

// promptIDFromButtons recovers the promptID foci encoded into each button's
// Data ("<promptID>:<index>", colon-free promptID). All buttons share it; the
// first suffices. Falls back to a fresh ULID if buttons are absent (defensive —
// a prompt always carries at least Allow/Deny).
func promptIDFromButtons(buttons []platform.ButtonChoice) string {
	if len(buttons) > 0 {
		if i := strings.IndexByte(buttons[0].Data, ':'); i >= 0 {
			return buttons[0].Data[:i]
		}
	}
	return fap.NewULID()
}

// toChoices maps foci's ButtonChoice slice onto the wire Choice slice.
func toChoices(buttons []platform.ButtonChoice) []fap.Choice {
	if len(buttons) == 0 {
		return nil
	}
	out := make([]fap.Choice, 0, len(buttons))
	for _, b := range buttons {
		out = append(out, fap.Choice{Label: b.Label, Data: b.Data, Row: b.Row})
	}
	return out
}

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
	sink := newAppSink(b)
	if c.agentRef != nil {
		sk := env.SessionKey
		sink.statusFn = func() (*int, string, string) { return c.agentRef.MetaStatus(sk) }
	}
	return sink, sink.cleanup
}

func (c *appConn) Connection() platform.Connection { return c }
