package slack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// enqueuer is the agent-side half of an agentCore (the platform.MessageHandler
// the gateway passes at SetupAgentConnection); narrowed to the one method the
// inbound path uses so tests drive a fake.
type enqueuer interface {
	Enqueue(agent.Envelope) bool
}

// inboxAgent is the full surface SetupAgentConnection needs from the agent:
// take envelopes, adopt the configured steer mode, and start the per-session
// inbox workers. *agent.Agent satisfies it.
type inboxAgent interface {
	enqueuer
	SetInboxSteerMode(bool)
	StartInbox(context.Context)
}

// agentCore is one agent's Slack state: its identity, its resolved allowlist
// and display settings, and handles onto the shared API client and target
// store. Conns are views over a core; the core is what SetupAgentConnection
// registers.
type agentCore struct {
	agentID        string
	api            *webAPI
	targets        *targetStore
	enq            enqueuer
	access         config.AllowedAccess
	accessMap      map[string]bool
	streamOutput   bool
	streamInterval time.Duration
	injectedHeader string
	onUserMessage  func()
	logger         func() *log.ComponentLogger
}

// userAllowed applies the interim access rule (#2459 A1, req 7): the existing
// allowlist cascade, resolved for this agent — so with no list configured
// nobody is allowed. Part A2 replaces the default.
func (c *agentCore) userAllowed(userID string) bool {
	return platform.UserAllowed(c.accessMap, c.access.Only, userID)
}

// fireOnUserMessage runs the platform lifecycle hook (the periodic runner's
// receipt stamp) when one is wired.
func (c *agentCore) fireOnUserMessage() {
	if c.onUserMessage != nil {
		c.onUserMessage()
	}
}

// targetFor resolves the conversation a session key's messages belong in.
// Branch keys derive the root's chat ID, so a branch of a Slack session
// delivers into the same conversation.
func (c *agentCore) targetFor(sessionKey string) (Target, bool) {
	if session.AgentIDFromKey(sessionKey) != c.agentID {
		return Target{}, false
	}
	return c.targets.lookup(c.agentID, session.ChatIDFromKey(sessionKey))
}

// targetOrDrop resolves the target of a session-blind send. what names the
// send in the drop log; the bool is false when there is no target (unbound
// connection or unregistered session): the send is dropped, never posted to
// a guessed conversation (#2459 req 5, 6).
func (c *agentCore) targetOrDrop(sessionKey, what string) (Target, bool) {
	t, ok := c.targetFor(sessionKey)
	if !ok {
		if sessionKey == "" {
			c.logger().Warnf("dropped session-blind %s (no session to address; Slack conversations are never default chats)", what)
		} else {
			c.logger().Warnf("dropped %s for session %s (no Slack conversation registered for it)", what, sessionKey)
		}
		return Target{}, false
	}
	return t, true
}

// Conn is the Slack platform.Connection. The stored per-agent instance
// (PrimaryBot) is UNBOUND (bound == ""): it is what agent-wide traffic
// reaches, and it drops every session-blind send. Manager.BotForSession
// mints a bound view pinned to one session, so every session-blind send —
// turn replies, streamed edits, notifications, ask prompts, files — lands in
// that session's own conversation (the app's BotForSession model, not
// Telegram/Discord's default-chat one; #2275's bug family cannot recur).
type Conn struct {
	core  *agentCore
	bound string
}

// boundTo returns a shallow copy pinned to sessionKey. The copy is a
// transient view — nobody stores it.
func (c *Conn) boundTo(sessionKey string) *Conn {
	view := *c
	view.bound = sessionKey
	return &view
}

// Compile-time capability checks. Deliberately NOT SessionButtonSender or
// SessionNotifier: a bound view already addresses its session, so the
// session-chat resolvers return it unchanged (same as the app).
var (
	_ platform.Connection           = (*Conn)(nil)
	_ platform.ButtonSender         = (*Conn)(nil)
	_ platform.SessionMessageEditor = (*Conn)(nil)
	_ platform.ApprovalRefuser      = (*Conn)(nil)
	_ platform.AgentWideDropper     = (*Conn)(nil)
	_ platform.ReplyConfiner        = (*Conn)(nil)
	_ agent.Driver                  = (*Conn)(nil)
)

// RefusesApprovals implements platform.ApprovalRefuser: Slack cannot show a
// tool-approval prompt to a human who may approve it (the requester could
// approve their own tool use), so approvals raised by a Slack session fail
// closed — denied, logged, noticed (#2459 A1; part C routes them to an
// operator and deletes this). True on the unbound instance too.
func (c *Conn) RefusesApprovals() bool { return true }

// DropsAgentWideSends implements platform.AgentWideDropper: the UNBOUND
// connection has no conversation an agent-wide send could address (Slack
// conversations are never default chats, #2459 req 6), so broadcast fan-outs
// skip it instead of addressing a certain drop. A bound view has its session
// and does not drop.
func (c *Conn) DropsAgentWideSends() bool { return c.bound == "" }

// ConfinesReplies implements platform.ReplyConfiner: every Slack
// conversation belongs to one user (a DM) or one thread's readers, so a
// session's reply text stays in that session's own conversation — even an
// explicit agent-wide broadcast must not copy it to another conversation or
// platform (#2459 req 5). True on the unbound instance too.
func (c *Conn) ConfinesReplies() bool { return true }

// --- identity ---

func (c *Conn) PlatformName() string { return platformName }
func (c *Conn) Username() string     { return c.core.agentID }

// --- session management ---

// SessionKey returns the bound session's key, "" on the unbound instance —
// the same contract the app's views carry (send_to_chat's cross-session
// header depends on it).
func (c *Conn) SessionKey() string { return c.bound }

// DefaultSessionKey: a bound view addresses its own session; the unbound
// instance has no default chat at all.
func (c *Conn) DefaultSessionKey() string { return c.bound }

// SetSessionKey / SetSessionKeyDirect / SetChatID satisfy the interface but
// are no-ops: a view's session is fixed at mint time, never mutated.
func (c *Conn) SetSessionKey(string)       {}
func (c *Conn) SetSessionKeyDirect(string) {}
func (c *Conn) SetChatID(int64)            {}

func (c *Conn) ChatID() int64 { return session.ChatIDFromKey(c.bound) }

// SessionKeyForChat derives the session key of a chat ID — pure derivation,
// no registration on a read path.
func (c *Conn) SessionKeyForChat(chatID int64) string {
	return session.NewChatSessionKey(c.core.agentID, chatID)
}

// cleanText applies the two cross-transport text filters every outbound
// message passes (StripSpuriousPrefix removes an assistant-injected leading
// marker; StripSilencingSuffix drops a trailing silencer). One helper so no
// send path can forget one of them.
func cleanText(text string) string {
	return platform.StripSilencingSuffix(platform.StripSpuriousPrefix(text))
}

// --- messaging ---

// deliverSessionText is the one preamble behind every session-addressed
// outbound text send (SendText, the chat-addressed sends, SendToSession,
// the notification pair): the transport text filters first — text that
// filters to empty sends nothing and resolves nothing — then the target for
// sessionKey (a session with no registered conversation drops with a WARN
// naming what), then the post. ok=false when nothing was sent.
func (c *Conn) deliverSessionText(sessionKey, text, what string) (msgRef, bool, error) {
	clean := cleanText(text)
	if clean == "" {
		return msgRef{}, false, nil
	}
	t, ok := c.core.targetOrDrop(sessionKey, what)
	if !ok {
		return msgRef{}, false, errNoSessionAddress
	}
	ref, err := c.core.post(context.Background(), t, clean)
	if err != nil {
		return msgRef{}, false, err
	}
	return ref, true, nil
}

func (c *Conn) SendText(text string) error {
	_, _, err := c.deliverSessionText(c.bound, text, "SendText")
	return err
}

func (c *Conn) SendTextToChat(chatID int64, text string) error {
	if !c.chatGuard(chatID, "SendTextToChat") {
		return errNoSessionAddress
	}
	_, _, err := c.deliverSessionText(c.SessionKeyForChat(chatID), text, "SendTextToChat")
	return err
}

// chatGuard refuses a chat-addressed send on a BOUND view for any chat other
// than the view's own session's: no send of one session may land in another
// session's conversation (#2459 req 5), so a chat id naming any other
// conversation of the agent is refused, not looked up. The unbound instance
// has no session and accepts any registered chat of the agent (the shape
// send_to_chat's cross-session header exists for).
func (c *Conn) chatGuard(chatID int64, what string) bool {
	if c.bound != "" && chatID != c.ChatID() {
		c.core.logger().Warnf("dropped %s for chat %d (session %s's conversation is the only one it may address)", what, chatID, c.bound)
		return false
	}
	return true
}

// sessionGuard is chatGuard for the methods that take an explicit session
// key (SendToSession, SendInjectedMessage, SendEditableInjected,
// EditInjectedMessage; review F2): on a BOUND view, a key naming another
// conversation is refused and logged, never looked up. The comparison is by
// conversation (agent + chat id), so a branch of the view's own session —
// which BotForSession would resolve to this same conversation — still sends.
// The unbound instance accepts any registered session of the agent.
func (c *Conn) sessionGuard(sessionKey, what string) bool {
	if c.bound == "" {
		return true
	}
	if session.AgentIDFromKey(sessionKey) != session.AgentIDFromKey(c.bound) || session.ChatIDFromKey(sessionKey) != c.ChatID() {
		c.core.logger().Warnf("dropped %s for session %s (session %s's conversation is the only one this view may address)", what, sessionKey, c.bound)
		return false
	}
	return true
}

// chatTarget resolves the conversation a chat-addressed send goes to: the
// guard above, then the session-keyed lookup.
func (c *Conn) chatTarget(chatID int64, what string) (Target, bool) {
	if !c.chatGuard(chatID, what) {
		return Target{}, false
	}
	return c.core.targetOrDrop(c.SessionKeyForChat(chatID), what)
}

// SendToSession delivers agent-reply text to a session's own conversation,
// without any injection header. A session with no registered Slack
// conversation is an error (the send was not delivered), matching the other
// transports' contract — callers such as send_to_session report the truth.
func (c *Conn) SendToSession(sessionKey, text string) error {
	return c.sendToSession(sessionKey, text, "SendToSession")
}

func (c *Conn) sendToSession(sessionKey, text, what string) error {
	if !c.sessionGuard(sessionKey, what) {
		return errForeignSession
	}
	_, _, err := c.deliverSessionText(sessionKey, text, what)
	return err
}

// SendInjectedMessage delivers a system-injected notice (wake results,
// reminders) to a session's conversation, with the configured
// injected_message_header prepended (empty disables; same display setting the
// other chat transports honour).
func (c *Conn) SendInjectedMessage(sessionKey, text string) error {
	return c.sendToSession(sessionKey, c.withInjectedHeader(text), "SendInjectedMessage")
}

// withInjectedHeader prepends the configured injected_message_header to a
// non-empty text; an empty header leaves the text unchanged.
func (c *Conn) withInjectedHeader(text string) string {
	if c.core.injectedHeader != "" && text != "" {
		return c.core.injectedHeader + "\n" + text
	}
	return text
}

func (c *Conn) SendNotification(text string) {
	c.sendNotification(text)
}

// SendNotificationDirect posts immediately and returns the platform message
// id ("<channel>:<ts>") so a later EditMessageText can rewrite it in place
// (the compaction ⏳→✅ flow). "" when nothing was sent.
func (c *Conn) SendNotificationDirect(text string) string {
	return c.sendNotification(text)
}

func (c *Conn) sendNotification(text string) string {
	ref, sent, err := c.deliverSessionText(c.bound, text, "notification")
	// A drop is already logged by targetOrDrop; only a failed post warns here.
	if err != nil && !errors.Is(err, errNoSessionAddress) {
		c.core.logger().Warnf("notification: %v", err)
	}
	if !sent {
		return ""
	}
	return ref.String()
}

// SetTyping is a no-op: Socket Mode bots have no typing indicator.
func (c *Conn) SetTyping(bool) {}

// --- files ---

// Each Sender media method uploads through the external-upload flow into the
// connection's own conversation (bound view) or the named chat's target.
// Slack has no separate voice/photo wire formats — everything is a file.

func (c *Conn) SendDocument(path, caption string) error {
	return c.sendMedia(c.bound, path, caption, "SendDocument")
}
func (c *Conn) SendVoice(path string) error { return c.SendDocument(path, "") }
func (c *Conn) SendVideo(path, caption string) error {
	return c.sendMedia(c.bound, path, caption, "SendVideo")
}
func (c *Conn) SendPhoto(path, caption string) error {
	return c.sendMedia(c.bound, path, caption, "SendPhoto")
}
func (c *Conn) SendAudio(path, caption string) error {
	return c.sendMedia(c.bound, path, caption, "SendAudio")
}
func (c *Conn) SendAnimation(path, caption string) error {
	return c.sendMedia(c.bound, path, caption, "SendAnimation")
}
func (c *Conn) SendVoiceData(audioData []byte) error {
	return c.sendMediaData(c.bound, "voice-note.mp3", audioData, "", "SendVoiceData")
}

func (c *Conn) SendDocumentToChat(chatID int64, path, caption string) error {
	return c.sendMediaToChat(chatID, path, caption, "SendDocumentToChat")
}
func (c *Conn) SendVoiceToChat(chatID int64, path string) error {
	return c.sendMediaToChat(chatID, path, "", "SendVoiceToChat")
}
func (c *Conn) SendVideoToChat(chatID int64, path, caption string) error {
	return c.sendMediaToChat(chatID, path, caption, "SendVideoToChat")
}
func (c *Conn) SendPhotoToChat(chatID int64, path, caption string) error {
	return c.sendMediaToChat(chatID, path, caption, "SendPhotoToChat")
}
func (c *Conn) SendAudioToChat(chatID int64, path, caption string) error {
	return c.sendMediaToChat(chatID, path, caption, "SendAudioToChat")
}
func (c *Conn) SendAnimationToChat(chatID int64, path, caption string) error {
	return c.sendMediaToChat(chatID, path, caption, "SendAnimationToChat")
}
func (c *Conn) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	t, ok := c.chatTarget(chatID, "SendVoiceDataToChat")
	if !ok {
		return errNoSessionAddress
	}
	return c.core.upload(context.Background(), t, "voice-note.mp3", audioData, "")
}

// sendMediaTarget reads the file at path and uploads it to t's
// conversation — the one read-and-upload body behind every path-based media
// send. The caller resolves the target BEFORE this runs: with nowhere to
// send, nothing touches disk.
func (c *Conn) sendMediaTarget(t Target, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.core.upload(context.Background(), t, filepath.Base(path), data, caption)
}

// sendMediaToChat uploads the file at path to the chat's conversation.
func (c *Conn) sendMediaToChat(chatID int64, path, caption, what string) error {
	t, ok := c.chatTarget(chatID, what)
	if !ok {
		return errNoSessionAddress
	}
	return c.sendMediaTarget(t, path, caption)
}

// sendMedia uploads the file at path, addressed by sessionKey (the bound
// view's own session).
func (c *Conn) sendMedia(sessionKey, path, caption, what string) error {
	t, ok := c.core.targetOrDrop(sessionKey, what)
	if !ok {
		return errNoSessionAddress
	}
	return c.sendMediaTarget(t, path, caption)
}

func (c *Conn) sendMediaData(sessionKey, name string, data []byte, caption, what string) error {
	t, ok := c.core.targetOrDrop(sessionKey, what)
	if !ok {
		return errNoSessionAddress
	}
	return c.core.upload(context.Background(), t, name, data, caption)
}

// --- platform.ButtonSender ---

// SendTextWithButtons posts the prompt body with one actions block per row;
// every block_id carries the target session, so the press routes back to it.
func (c *Conn) SendTextWithButtons(text string, buttons []platform.ButtonChoice, callbackPrefix string) (string, error) {
	t, ok := c.core.targetOrDrop(c.bound, "button prompt")
	if !ok {
		return "", errNoSessionAddress
	}
	ref, err := c.core.postButtons(context.Background(), t, text, buttons, callbackPrefix, c.bound)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

// EditMessageText replaces a message's text and strips its buttons. msgID is
// the "<channel>:<ts>" this connection returned, so the edit is
// self-addressed; the unbound connection — which has no session whose
// conversation an edit could be checked against — refuses every edit, and on
// a bound view an id naming another conversation is refused: defence in
// depth against a misrouted id.
func (c *Conn) EditMessageText(msgID, text string) error {
	return c.editRef(msgID, text, nil, "")
}

// EditMessageWithButtons replaces text and buttons in place.
func (c *Conn) EditMessageWithButtons(msgID, text string, buttons []platform.ButtonChoice, callbackPrefix string) error {
	return c.editRef(msgID, text, buttons, callbackPrefix)
}

// guardEditRef checks that a message id names a message in sessionKey's own
// conversation before an edit is attempted — defence in depth against a
// misrouted id editing another conversation's message (#2459 req 5). The
// conversation is the full channel AND thread: two threads of one channel
// are different conversations. It fails closed: a session with no resolvable
// Slack conversation (never registered, or another agent's) has nothing to
// compare and no business editing anything, so it is refused like a
// mismatch, never waved through.
func (c *Conn) guardEditRef(sessionKey string, ref msgRef) error {
	t, ok := c.core.targetFor(sessionKey)
	if !ok {
		return fmt.Errorf("slack: refusing to edit message %s for session %s (no Slack conversation registered for it)", ref, sessionKey)
	}
	if t.Channel != ref.Channel || t.threadTS() != ref.ThreadTS {
		return fmt.Errorf("slack: refusing to edit message %s from session %s (different conversation)", ref, sessionKey)
	}
	return nil
}

func (c *Conn) editRef(msgID, text string, buttons []platform.ButtonChoice, callbackPrefix string) error {
	ref, err := parseEditRef(msgID)
	if err != nil {
		return err
	}
	if c.bound == "" {
		// The unbound connection has no session whose conversation an edit
		// could be checked against, and every legitimate edit goes through
		// a session's own bound view — so this fails closed rather than
		// edit an unsupervised conversation (#2459 req 5).
		return fmt.Errorf("slack: refusing to edit message %s on the unbound connection (no session to confine the edit to)", ref)
	}
	if err := c.guardEditRef(c.bound, ref); err != nil {
		return err
	}
	if buttons != nil {
		blocks, err := buttonBlocks(text, buttons, callbackPrefix, c.bound)
		if err != nil {
			return err
		}
		return c.core.api.updateMessage(context.Background(), updateParams{Channel: ref.Channel, TS: ref.TS, Text: escapeText(text), Blocks: blocks})
	}
	return c.core.editText(context.Background(), ref, text)
}

// --- platform.SessionMessageEditor (#2384) ---

// SendEditableInjected posts text to a session's conversation, with the
// injected_message_header prepended like SendInjectedMessage (#3003), and
// returns the id EditInjectedMessage rewrites in place. It rides the one
// session-text preamble (deliverSessionText) and translates its outcomes
// into this surface's contract: an empty message and an unroutable session
// are both errors — the caller asked for a return id, so "nothing was sent"
// must never masquerade as success.
func (c *Conn) SendEditableInjected(sessionKey, text string) (string, error) {
	if !c.sessionGuard(sessionKey, "editable message") {
		return "", errForeignSession
	}
	if cleanText(text) == "" {
		// Checked before the header goes on: a header alone is not a message.
		return "", fmt.Errorf("editable message for session %q is empty", sessionKey)
	}
	ref, sent, err := c.deliverSessionText(sessionKey, c.withInjectedHeader(text), "editable message")
	switch {
	case err != nil:
		if errors.Is(err, errNoSessionAddress) {
			return "", fmt.Errorf("no Slack conversation for session %q", sessionKey)
		}
		return "", err
	case !sent:
		return "", fmt.Errorf("editable message for session %q is empty", sessionKey)
	}
	return ref.String(), nil
}

// EditInjectedMessage rewrites a SendEditableInjected message in place. The
// id must name a message in sessionKey's own conversation — channel and
// thread — an id from another conversation is refused, never edited (req 5:
// no send of one session may touch another session's conversation).
func (c *Conn) EditInjectedMessage(sessionKey, msgID, text string) error {
	if !c.sessionGuard(sessionKey, "EditInjectedMessage") {
		return errForeignSession
	}
	clean := cleanText(text)
	if clean == "" {
		return fmt.Errorf("edit of message %s is empty", msgID)
	}
	clean = c.withInjectedHeader(clean)
	ref, err := parseEditRef(msgID)
	if err != nil {
		return err
	}
	if err := c.guardEditRef(sessionKey, ref); err != nil {
		return err
	}
	return c.core.editText(context.Background(), ref, clean)
}

// --- agent.Driver ---

// Connection implements agent.Driver.
func (c *Conn) Connection() platform.Connection { return c }
