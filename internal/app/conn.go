package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
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
	Enqueue(agent.Envelope) bool
	MetaStatus(sessionKey string) (gap string)
}

type appConn struct {
	hub      *Hub
	agentID  string
	agentRef agentCore
	commands *command.Registry
	cmdCtx   command.CommandContext
	stt      voice.STT // inbound voice transcription; nil = unsupported

	// Platform lifecycle callbacks, wired by the gateway via
	// SetLifecycleCallback (mirror of telegram.Bot's hooks). OnUserMessage
	// fires on each inbound user message/command (drives the periodic runner's
	// lastInteraction — reflection/consolidation/reset-idle-guard gate on it);
	// OnTurnComplete/OnTurnEnd fire at turn completion (cache-warm + warning
	// flush). Stored on the per-agent PrimaryBot instance; set once at startup,
	// read concurrently during turns — the same set-once contract as telegram.
	OnUserMessage  func()
	OnTurnComplete func()
	OnTurnEnd      func()

	// bound is the session this view is pinned to. The stored per-agent instance
	// (PrimaryBot) leaves it empty; hub.BotForSession mints a shallow copy with
	// bound set, so a session-blind send (SendText/SendNotification/media) routes
	// to the right socket without a mutable "default session". An empty bound is
	// the agent-wide instance: session-blind notifications fan out to every live
	// binding rather than guessing whoever-spoke-last (the wrong-chat bug).
	bound string
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

func (c *appConn) SessionKey() string { return c.bound }

func (c *appConn) DefaultSessionKey() string        { return c.bound }
func (c *appConn) DefaultSessionKeyOrEmpty() string { return c.bound }

// InvokeTool routes an agent tool call to a connected device via FAP
// `tool.invoke`. Delegates to the Hub, which finds a live wsClient for this
// agent, registers a pending caller, and awaits the matching `tool.result`.
// Returns ErrNoLiveDevice if the agent has no connected app device.
//
// The Hub is the routing authority (it owns the agent→client map); the appConn
// is just the per-agent handle the tool layer reaches via connMgr.
func (c *appConn) InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (fap.ToolResult, error) {
	return c.hub.InvokeTool(ctx, c.agentID, tool, action, args)
}

// SetSessionKey / SetSessionKeyDirect / SetChatID satisfy platform.Connection
// but are no-ops for the app: a view's session is fixed at mint time (the
// immutable bound field), never mutated in place. Only the /fork facet path
// calls these, and the app has no facets yet (AcquireFacet returns false).
func (c *appConn) SetSessionKey(string)       {}
func (c *appConn) SetSessionKeyDirect(string) {}
func (c *appConn) SetChatID(int64)            {}

func (c *appConn) ChatID() int64 { return session.ChatIDFromKey(c.bound) }

func (c *appConn) SessionKeyForChat(chatID int64) string {
	return c.hub.sessionKeyForChat(c.agentID, chatID)
}

// --- messaging ---

func (c *appConn) SendToSession(sessionKey, text string) error {
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if clean == "" {
		return nil
	}
	b := c.sendBinding(sessionKey)
	if b == nil {
		return nil
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
	b := c.sendBinding(sessionKey)
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

// sendBinding resolves the target binding for an unsolicited send. When
// sessionKey has no binding — an agent-initiated/independent session the app
// never opened — it falls back to the agent's default app chat so the message
// surfaces instead of vanishing (#959). It logs LOUDLY either way, so the
// never-bound case is never a silent recovery and we can see how often it fires.
func (c *appConn) sendBinding(sessionKey string) *convBinding {
	if b := c.hub.bindingForSession(sessionKey); b != nil {
		return b
	}
	// No binding for this session — resolve (or create) the agent's default
	// conversation. The app can mint conversations freely, so an unsolicited
	// send always has somewhere sensible to land (see Hub.deliverBinding).
	b, via := c.hub.deliverBinding(c.agentID)
	if b == nil {
		log.Warnf("app", "unsolicited send to unbound session %q (agent %s) DROPPED: conversation creation failed", sessionKey, c.agentID)
		return nil
	}
	if sessionKey != "" {
		log.Infof("app", "unsolicited send to unbound session %q (agent %s) routed to %s conversation %s", sessionKey, c.agentID, via, b.convID)
	}
	return b
}

func (c *appConn) SendText(text string) error {
	return c.SendToSession(c.SessionKey(), text)
}

func (c *appConn) SendTextToChat(chatID int64, text string) error {
	return c.SendToSession(c.SessionKeyForChat(chatID), text)
}

func (c *appConn) SendNotification(text string) { c.notify(text) }

// SendNotificationDirect returns the delivered notification's messageID so the
// caller can later edit it in place via EditMessageText (the compaction ⏳→✅
// flow). Empty when nothing was delivered or this is the unbound fan-out
// instance (agent-wide notices like rate-limit warnings are not edited).
func (c *appConn) SendNotificationDirect(text string) string {
	return c.notify(text)
}

// notify delivers an agent notification and returns its messageID. A bound
// view targets its pinned session; the unbound stored instance (broadcast
// warnings, AllForAgent fallbacks in the compaction/tasklist hooks) targets
// the agent's default conversation via the same resolve-or-create ladder as
// every other session-blind send (Hub.deliverBinding) — one destination, not
// a fan-out to every conversation. Session-targeted notices that must land
// in a SPECIFIC chat use a bound view (the wrong-chat compaction-notice bug,
// #911). The messageID is registered so a later EditMessageText can replace
// the notification in place.
func (c *appConn) notify(text string) string {
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if clean == "" {
		return ""
	}
	var b *convBinding
	if c.bound != "" {
		b = c.hub.bindingForSession(c.bound)
	} else {
		b, _ = c.hub.deliverBinding(c.agentID)
	}
	if b == nil {
		return ""
	}
	msgID := fap.NewULID()
	c.hub.registerNotification(msgID, b)
	b.send(fap.Notification{ConversationID: b.convID, MessageID: msgID, Text: clean, Level: "info"})
	return msgID
}

// SetTyping is intentionally a no-op for the app. The app's activity indicator is
// owned exclusively by appSink, which brackets each turn with a single
// fap.Activity{Kind:"warming"} at TurnStart and {Kind:"idle"} at TurnComplete
// (guaranteed via defer, even on error). The platform TypingFunc path — designed for Telegram/
// Discord's refresh-and-auto-expire SetChatAction — must NOT drive the app, or its
// periodic re-asserts (refresh `true`s) and intermediate cancels (round-end `false`s)
// leak through as redundant frames and mid-session flicker. App clients have no
// auto-expire, so every frame is literal and must be paired exactly once per turn.
func (c *appConn) SetTyping(bool) {}

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
	// The app's click dispatch deletes the prompt binding on any resolved
	// callback, so a non-terminal toggle would orphan the remaining buttons.
	// Drop toggle buttons here; the app shows only the terminal choices.
	buttons = dropToggleButtons(buttons)
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
// binding, or the app never advertised the capability — so the ask layer falls
// back to the sequential SendTextWithButtons path. A capable app that's merely
// offline still batches: the cached capability keeps supportsFeature true and the
// frame persists for replay on reconnect. Implements platform.BatchButtonSender.
func (c *appConn) SendInteractiveBatch(promptID string, questions []platform.BatchQuestion, onResponse func(answers []string)) (bool, error) {
	b := c.hub.bindingForSession(c.SessionKey())
	if b == nil {
		return false, nil // no binding for this conv → sequential path
	}
	if !b.supportsFeature(featureInteractiveBatch) {
		return false, nil // app never advertised batch support → sequential one-at-a-time
	}
	c.hub.registerBatchPrompt(promptID, b, onResponse)
	qs := make([]fap.Question, len(questions))
	choiceCounts := make([]string, len(questions))
	for i, q := range questions {
		qs[i] = fap.Question{Text: q.Text, Header: q.Header, Choices: toChoices(q.Choices)}
		choiceCounts[i] = strconv.Itoa(len(q.Choices))
	}
	log.Debugf("app", "SendInteractiveBatch: conv=%s prompt=%s questions=%d choicesPerQ=[%s]",
		b.convID, promptID, len(qs), strings.Join(choiceCounts, ","))
	b.send(fap.Interactive{
		ConversationID: b.convID,
		PromptID:       promptID,
		Questions:      qs,
		ExpiresAt:      time.Now().Add(defaultPromptTTL).Format(time.RFC3339),
	})
	return true, nil
}

// EditMessageText edits a previously-sent message in place. msgID is either a
// promptID (from SendTextWithButtons → replace text, drop buttons) or a
// notification messageID (from SendNotificationDirect → re-send the notification
// with the same id so the client replaces the row, e.g. compaction ⏳→✅).
// Idempotent — an unknown id (already resolved/consumed) is a no-op.
func (c *appConn) EditMessageText(msgID, text string) error {
	if b := c.hub.bindingForPrompt(msgID); b != nil {
		c.hub.deletePrompt(msgID)
		b.send(fap.InteractiveEdit{ConversationID: b.convID, PromptID: msgID, Text: text})
		return nil
	}
	if b := c.hub.bindingForNotification(msgID); b != nil {
		c.hub.deleteNotification(msgID)
		clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
		if clean != "" {
			// Same messageID → the client upserts (replaces) the existing row.
			b.send(fap.Notification{ConversationID: b.convID, MessageID: msgID, Text: clean, Level: "info"})
		}
		return nil
	}
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

// dropToggleButtons returns buttons with non-terminal toggle buttons removed.
func dropToggleButtons(buttons []platform.ButtonChoice) []platform.ButtonChoice {
	out := buttons[:0:0]
	for _, b := range buttons {
		if b.Toggle == nil {
			out = append(out, b)
		}
	}
	return out
}

// toChoices maps foci's ButtonChoice slice onto the wire Choice slice.
func toChoices(buttons []platform.ButtonChoice) []fap.Choice {
	if len(buttons) == 0 {
		return nil
	}
	out := make([]fap.Choice, 0, len(buttons))
	for _, b := range buttons {
		out = append(out, fap.Choice{Label: b.Label, Data: b.Data, Row: b.Row, Description: b.Description})
	}
	return out
}

// --- agent.Driver ---

// WrapTurn runs the turn and fires the platform lifecycle hooks. Typing
// lifecycle is handled by the streaming sink (TurnStart/TurnComplete), but the
// gateway-level hooks (cache-warm + warning flush) belong here — same shape as
// telegram.Bot.WrapTurn. OnTurnComplete fires once fn returns (success or
// error); OnTurnEnd fires last in the deferred cleanup.
func (c *appConn) WrapTurn(ctx context.Context, fn func() error) error {
	defer func() {
		if c.OnTurnEnd != nil {
			c.OnTurnEnd()
		}
	}()
	err := fn()
	if c.OnTurnComplete != nil {
		c.OnTurnComplete()
	}
	return err
}

// NewTurnSink builds the per-turn streaming sink bound to the envelope's
// conversation. Returns a nil sink if no live binding exists for the session
// (the socket dropped) — the agent skips rendering in that case.
func (c *appConn) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	b := c.hub.bindingForSession(env.SessionKey)
	if b == nil {
		return nil, nil
	}
	// No per-turn "default session" stamp here: the sink is bound directly to
	// this envelope's binding (b), and async notifications resolve their own
	// session via hub.BotForSession. Stamping a shared default was last-speaker-
	// wins and misrouted compaction notices to whoever spoke most recently.
	sink := newAppSink(b)
	if c.agentRef != nil {
		sk := env.SessionKey
		sink.statusFn = func() string { return c.agentRef.MetaStatus(sk) }
	}
	return sink, sink.cleanup
}

func (c *appConn) Connection() platform.Connection { return c }
