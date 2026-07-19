package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turn"
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
	CacheExpiryMs(sessionKey string, at time.Time) int64
	TransformMessage(text string) string
}

// interactiveHeaderState holds pending headers for interactive prompts,
// protected by a mutex. Pointer-based on appConn so shallow clones share it.
type interactiveHeaderState struct {
	mu      sync.Mutex
	pending map[string]string
}

type appConn struct {
	hub      *Hub
	agentID  string
	agentRef agentCore
	commands *command.Registry
	cmdCtx   command.CommandContext
	stt      voice.STT // inbound voice transcription; nil = unsupported

	// Pending interactive headers, set by SetInteractiveHeader and consumed
	// by SendTextWithButtons. Keyed by prompt ID. Pointer-based so the
	// shallow clone in BotForSession shares the same map (the clone is
	// the same connection, just session-bound).
	headerState *interactiveHeaderState

	// Platform lifecycle callbacks, wired by the gateway via
	// SetLifecycleCallback (mirror of telegram.Bot's hooks). OnUserMessage
	// fires on each inbound user message/command (drives the periodic runner's
	// lastInteraction — reflection/consolidation/reset-idle-guard gate on it).
	// Stored on the per-agent PrimaryBot instance; set once at startup, read
	// concurrently during turns — the same set-once contract as telegram. The
	// turn-boundary hooks (cache-warm, warning flush) moved to the Agent (see
	// Agent.SetTurnLifecycleHooks) so injected turns fire them too.
	OnUserMessage func()

	// bound is the session this view is pinned to. The stored per-agent instance
	// (PrimaryBot) leaves it empty; hub.BotForSession mints a shallow copy with
	// bound set, so a session-blind send (SendText/SendNotification/media) routes
	// to the right socket without a mutable "default session". An empty bound is
	// the agent-wide instance: session-blind notifications fan out to every live
	// binding rather than guessing whoever-spoke-last (the wrong-chat bug).
	bound string
}

var (
	_ platform.Connection           = (*appConn)(nil)
	_ platform.ButtonSender         = (*appConn)(nil)
	_ platform.BatchButtonSender    = (*appConn)(nil)
	_ agent.Driver                  = (*appConn)(nil)
	_ turn.SessionSubagentDeliverer = (*appConn)(nil)
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

// Compile-time guard: *appConn MUST satisfy tools.AppInvoker, because the
// app_android tool resolves its invoker via a runtime conn.(tools.AppInvoker)
// assertion (cmd/foci-gw/tool_table.go). A signature drift (e.g. a mirror
// return type) would silently fail that assertion at runtime and leave the tool
// reporting "no device connected" — this line turns that into a build error.
var _ tools.AppInvoker = (*appConn)(nil)

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
	b, fellBack := c.sendBinding(sessionKey)
	if b == nil {
		return nil
	}
	if fellBack {
		clean = misdeliveryBanner(sessionKey, b.convID, clean)
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
	b, fellBack := c.sendBinding(sessionKey)
	if b == nil {
		return nil
	}
	if fellBack {
		text = misdeliveryBanner(sessionKey, b.convID, text)
	}
	b.send(fap.ServerMessage{
		ConversationID: b.convID,
		MessageID:      fap.NewULID(),
		Role:           "system",
		Text:           text,
	})
	return nil
}

func (c *appConn) DeliverSubagentStartToSession(sessionKey, groupKey, label string, runIndex int, prompt string) {
	if b, _ := c.sendBinding(sessionKey); b != nil {
		b.send(fap.SubagentStart{ConversationID: b.convID, GroupKey: groupKey, Label: label, RunIndex: runIndex, Prompt: prompt})
	}
}

func (c *appConn) DeliverSubagentTextToSession(sessionKey, groupKey, text string, runIndex int) {
	if b, _ := c.sendBinding(sessionKey); b != nil {
		b.send(fap.SubagentText{ConversationID: b.convID, GroupKey: groupKey, Text: text, RunIndex: runIndex})
	}
}

func (c *appConn) DeliverSubagentEndToSession(sessionKey, groupKey string, runIndex int) {
	if b, _ := c.sendBinding(sessionKey); b != nil {
		b.send(fap.SubagentEnd{ConversationID: b.convID, GroupKey: groupKey, RunIndex: runIndex})
	}
}

// sendBinding resolves the target binding for an unsolicited send. When
// sessionKey has no binding — an agent-initiated/independent session the app
// never opened — it falls back to the agent's default app chat so the message
// surfaces instead of vanishing (#959). It logs LOUDLY either way, so the
// never-bound case is never a silent recovery and we can see how often it fires.
func (c *appConn) sendBinding(sessionKey string) (b *convBinding, fellBack bool) {
	if b := c.hub.bindingForSession(sessionKey); b != nil {
		return b, false
	}
	// No binding for this session — resolve (or create) the agent's default
	// conversation. The app can mint conversations freely, so an unsolicited
	// send always has somewhere sensible to land (see Hub.deliverBinding).
	b, via := c.hub.deliverBinding(c.agentID)
	if b == nil {
		appLog.Warnf("unsolicited send to unbound session %q (agent %s) DROPPED: conversation creation failed", sessionKey, c.agentID)
		return nil, false
	}
	if sessionKey != "" {
		// A session-TARGETED send whose own conversation couldn't be found: it's
		// being routed to a different conversation. fellBack=true so the caller
		// annotates it as misdelivered (a session-blind send, sessionKey=="",
		// has no intended target to miss — that's normal routing, not a miss).
		appLog.Infof("unsolicited send to unbound session %q (agent %s) routed to %s conversation %s", sessionKey, c.agentID, via, b.convID)
		return b, true
	}
	return b, false
}

// misdeliveryBanner wraps a message that could not reach its addressed session's
// own conversation and was routed to another (the deliverBinding fallback). Per
// Dick's spec (2026-07-11): a visible top-and-bottom banner naming the intended
// session and the conversation it actually landed in, so a misrouted message is
// never silently absorbed into the wrong chat. There is no "target convID" to
// cite — the fallback fires precisely because the session has no bound
// conversation — so the banner names the session key as the thing not found.
func misdeliveryBanner(sessionKey, deliveredConvID, text string) string {
	line := fmt.Sprintf("⚠ Addressed to session %s — no conversation for it could be found; delivered to %s instead.", sessionKey, deliveredConvID)
	const sep = "———"
	return line + "\n" + sep + "\n" + text + "\n" + sep + "\n" + line
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

// SetInteractiveHeader stores a header to be included in the next Interactive
// frame sent for this prompt ID. Implements platform.InteractiveHeaderSetter.
// Called by SendInteractiveMessageWithID before SendTextWithButtons.
func (c *appConn) SetInteractiveHeader(promptID, header string) {
	if c.headerState == nil {
		return
	}
	c.headerState.mu.Lock()
	c.headerState.pending[promptID] = header
	c.headerState.mu.Unlock()
}

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
	// Consume any pending header set by SetInteractiveHeader.
	var header string
	if c.headerState != nil {
		c.headerState.mu.Lock()
		header = c.headerState.pending[promptID]
		delete(c.headerState.pending, promptID)
		c.headerState.mu.Unlock()
	}
	c.hub.registerPrompt(promptID, b)
	b.send(fap.Interactive{
		ConversationID: b.convID,
		PromptID:       promptID,
		Text:           text,
		Header:         header,
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
	c.hub.registerBatchPrompt(promptID, b, len(questions), onResponse)
	qs := make([]fap.Question, len(questions))
	choiceCounts := make([]string, len(questions))
	for i, q := range questions {
		qs[i] = fap.Question{Text: q.Text, Header: q.Header, Choices: toChoices(q.Choices)}
		choiceCounts[i] = strconv.Itoa(len(q.Choices))
	}
	appLog.Debugf("SendInteractiveBatch: conv=%s prompt=%s questions=%d choicesPerQ=[%s]",
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

// AttachDetail implements platform.DetailAttacher. Registers `detail` as a
// blob (same store used for media/documents — fetched via GET
// /app/blob/<id>) and re-sends the notification with the same msgID plus
// the blob reference, so the client can upsert the row into a tappable
// chit. Mirrors EditMessageText's idempotency: an unknown/already-consumed
// msgID is a silent no-op.
func (c *appConn) AttachDetail(msgID, text, detail string) error {
	b := c.hub.bindingForNotification(msgID)
	if b == nil {
		return nil
	}
	meta, err := c.hub.blobs.putBytes([]byte(detail), fap.MediaDocument, "compaction-summary.md", "text/markdown")
	if err != nil {
		return err
	}
	c.hub.deleteNotification(msgID)
	clean := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
	if clean == "" {
		return nil
	}
	b.send(fap.Notification{ConversationID: b.convID, MessageID: msgID, Text: clean, Level: "info", DetailBlobID: meta.id})
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

// WrapTurn runs the turn. The app has no platform-delivery-hygiene state to
// manage (typing lifecycle is handled by the streaming sink's
// TurnStart/TurnComplete), and the session/keepalive hooks (cache-warm,
// warning flush) now fire at the turn boundary in Agent.HandleMessage (see
// Agent.SetTurnLifecycleHooks), so this is a straight pass-through kept only
// to satisfy agent.Driver.
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
	// No per-turn "default session" stamp here: the sink is bound directly to
	// this envelope's binding (b), and async notifications resolve their own
	// session via hub.BotForSession. Stamping a shared default was last-speaker-
	// wins and misrouted compaction notices to whoever spoke most recently.
	sink := newAppSink(b)
	if c.agentRef != nil {
		sk := env.SessionKey
		sink.statusFn = func() string { return c.agentRef.MetaStatus(sk) }
		sink.cacheExpiryFn = func() int64 { return c.agentRef.CacheExpiryMs(sk, time.Now()) }
	}
	return sink, sink.cleanup
}

func (c *appConn) Connection() platform.Connection { return c }
