package app

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"foci/internal/command"
	"foci/internal/delegator"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/tools"
)

// activeHub is the hub of the configured app provider, set at Init. The HTTP
// layer (cmd/foci-gw) reaches it through Enabled/WSHandler without importing
// provider internals or plumbing the API key around — the hub authenticates.
var (
	activeMu  sync.RWMutex
	activeHub *Hub
)

func setActiveHub(h *Hub) {
	activeMu.Lock()
	activeHub = h
	activeMu.Unlock()
}

// Enabled reports whether the app provider is configured and able to serve the
// app endpoints (a hub exists). Authentication is per-device-token (#862); there
// is no shared key whose presence gates the endpoint.
func Enabled() bool {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeHub != nil
}

// OpenSessionsForAgent returns the session keys of the agent's open chats from
// the persisted shared open-set (open_chats), or nil if the app provider is not
// running. Used by keepalive when warm_open_app_chats is set. Persisted rather
// than live-socket so keepalive keeps warming open chats across app disconnects.
func OpenSessionsForAgent(agentID string) []string {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return nil
	}
	return h.OpenSessionsForAgent(agentID)
}

// ActiveConnCount returns the number of live app sockets on the configured hub,
// or 0 if the app provider is not running. The goroutine monitor calls it each
// sample tick to grow its threshold with the live connection count (dynamic
// headroom for a quantity the startup formula cannot predict).
func ActiveConnCount() int {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return 0
	}
	return h.ConnCount()
}

// ExpireBatchPrompts resolves, as cancelled, every batched (app-form) ask on the
// configured hub that has been waiting longer than maxAge. No-op if the app
// provider is not running. Called from the same periodic sweep as
// platform.CleanupExpiredInteractive so both ask shapes share one TTL (#1895).
func ExpireBatchPrompts(maxAge time.Duration) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.expireBatchPrompts(time.Now().Add(-maxAge))
}

// PresentAskgwPrompt shows an ask-gateway question on every app device as an
// app-wide prompt, tied to no agent or conversation (#2707). onChoice receives
// the tapped button's Data, once, from whichever device answers first. Reports
// false — nothing shown — when the app provider is not running or no paired
// device has a build that can show it (none connected with the feature, none
// push-wakeable with it); the caller then posts to the agent chat. A device
// that is offline sees the prompt on its next connect. host is the host the
// ask came from (#2800), shown in the dialog; empty when unknown.
func PresentAskgwPrompt(id, header, text, host string, buttons []platform.ButtonChoice, onChoice func(data string)) bool {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return false
	}
	return h.presentAskgw(id, header, platform.StripEmbeddedHeader(header, text), host, buttons, onChoice)
}

// CloseAskgwPrompt withdraws an app-wide askgw prompt (cancelled or timed out)
// from every device. Reports whether it was pending; a no-op otherwise.
func CloseAskgwPrompt(id string) bool {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return false
	}
	return h.closeAskgw(id)
}

// MintActivePairKey mints a single-use, short-TTL pairing key on the live app
// hub (#862) and returns it with its expiry. A device exchanges this key at
// POST /app/pair for its own revocable token. The key lives only in memory and
// is delivered to the user out-of-band (the /android wizard or the `foci` CLI);
// it is never persisted or logged. Returns an error if the app provider is not
// running (pairing requires a live gateway).
func MintActivePairKey(ttl time.Duration) (string, time.Time, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", time.Time{}, errors.New("app provider is not running")
	}
	key, exp := h.pairKeys.mint(ttl)
	return key, exp, nil
}

// MarkMessageConsumed tells the app clients of conversation convID that the
// agent's backend consumed the user's message messageID — the ✓✓ tick (#2050).
// Sent as a durable conversation frame, so a device offline now still learns it
// on replay. No-op when the app provider isn't running, the ref is empty, or
// the conversation is unknown (a message from another platform).
func MarkMessageConsumed(convID, messageID string) {
	if convID == "" || messageID == "" {
		return
	}
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.mu.RLock()
	b := h.convs[convID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	b.send(fap.MessageConsumed{ConversationID: convID, MessageID: messageID})
}

// RecordMessageDelivery files how the app's user message messageID reached
// the agent (#2992): the agent's Delivery* outcome, the turn's ledger id, and
// when it was decided. No-op when the app provider is not running or the
// message did not come through it.
func RecordMessageDelivery(convID, messageID, outcome, turnID string, at time.Time) {
	if convID == "" || messageID == "" {
		return
	}
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.frames.PutUserDelivery(convID, messageID, outcome, turnID, at)
}

// SetSubagentDetail routes a running-subagent status detail (or "" when none
// are running) to the conversation bound to sessionKey, updating its unified
// Activity indicator. No-op when the app provider is not running or the session
// has no live binding — the app is only one of several platforms and a session
// may not be an app conversation at all.
func SetSubagentDetail(sessionKey, detail string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		b.setSubagentDetail(detail)
	}
}

// SetSubagents routes the structured running-subagent list for sessionKey to
// its conversation (#2138), with the one-line detail derived from it. Same
// no-op rules as SetSubagentDetail.
func SetSubagents(sessionKey string, running []delegator.RunningSubagent) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return
	}
	descs := make([]string, len(running))
	var list []fap.RunningSubagent
	for i, r := range running {
		descs[i] = r.Description
		var started int64
		if !r.Started.IsZero() {
			started = r.Started.UnixMilli()
		}
		list = append(list, fap.RunningSubagent{
			ID: r.ID, Description: r.Description, Kind: r.Kind,
			Model: r.Model, SubagentType: r.SubagentType, Command: r.Command, StartedMs: started,
		})
	}
	b.setSubagents(delegator.FormatSubagentDetail(descs), list)
}

// SpawnObserver shows each foci spawn in the app exactly as a Claude Code
// subagent of the session that spawned it (#2139; a direct spawn too, while
// its call waits, #2683): the same subagent
// start/text/end frames, and an "agent"-kind entry in the activity frame's
// running list, so the client needs no spawn-specific code. A session no app conversation is
// bound to is skipped; unlike a reply, a spawn's progress has nowhere better to
// go.
type SpawnObserver struct{}

var _ tools.SpawnObserver = SpawnObserver{}

func spawnBinding(sessionKey string) *convBinding {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return nil
	}
	return h.bindingForSession(sessionKey)
}

// SpawnStarted opens the spawn's chit with its prompt and lists it as running.
func (SpawnObserver) SpawnStarted(parentSession string, s tools.RunningSpawn, prompt string) {
	b := spawnBinding(parentSession)
	if b == nil {
		return
	}
	// The app labels a spawn by its name (#2721), not by its prompt, which
	// the view still shows as its first message.
	label := s.Label()
	b.send(fap.SubagentStart{ConversationID: b.convID, GroupKey: s.ID, Label: label, RunIndex: 1, Prompt: prompt})
	var started int64
	if !s.Started.IsZero() {
		started = s.Started.UnixMilli()
	}
	b.addSpawn(fap.RunningSubagent{ID: s.ID, Description: label, Kind: "agent", Model: s.Model, SubagentType: "spawn", StartedMs: started}, s.CallerShell)
}

// SpawnText adds one block of the spawn's text to its chit.
func (SpawnObserver) SpawnText(parentSession, id, text string) {
	NoteSubagentActivity(parentSession, id, "text", text)
	if b := spawnBinding(parentSession); b != nil {
		b.send(fap.SubagentText{ConversationID: b.convID, GroupKey: id, Text: text, RunIndex: 1})
	}
}

// SpawnTool records one tool call of the spawn as its activity, for the
// Stop-view summary (#2960). Nothing is shown in the chit.
func (SpawnObserver) SpawnTool(parentSession, id, line string) {
	NoteSubagentActivity(parentSession, id, "tool", line)
}

// SpawnPrompt adds a parent's spawn_send message to the spawn's running chit
// (#2822), as the SubagentPrompt frame a Claude Code SendMessage to a running
// subagent gets (#1419): another "asked" block in the current run.
func (SpawnObserver) SpawnPrompt(parentSession, id, message string) {
	if b := spawnBinding(parentSession); b != nil {
		b.send(fap.SubagentPrompt{ConversationID: b.convID, GroupKey: id, Prompt: message, RunIndex: 1})
	}
}

// SpawnModel names the model on the spawn's running entry (#2879).
func (SpawnObserver) SpawnModel(parentSession, id, model string) {
	if b := spawnBinding(parentSession); b != nil {
		b.setSpawnModel(id, model)
	}
}

// SpawnEnded completes the spawn's chit and drops it from the running list.
func (SpawnObserver) SpawnEnded(parentSession, id string) {
	NoteSubagentActivity(parentSession, id, "end", "")
	if b := spawnBinding(parentSession); b != nil {
		b.send(fap.SubagentEnd{ConversationID: b.convID, GroupKey: id, RunIndex: 1})
		b.removeSpawn(id)
	}
}

// SetCacheExpiry routes a prompt-cache expiry (unix ms; 0 = cold) to the
// conversation bound to sessionKey, refreshing its warmth indicator whenever
// the cache is (re)warmed — not only on a turn that completes through a live app
// sink. The agent's onCacheExpiry hook is wired here at gateway setup. No-op
// when the app provider isn't running or the session has no live binding.
func SetCacheExpiry(sessionKey string, expiryMs int64) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		b.setCacheExpiry(expiryMs)
	}
}

// DeliverPromptSuggestion sends the backend's predicted next prompt to the
// conversation bound to sessionKey (#2916). The frame is stored and replayed
// like any other, so a phone asleep when it was generated still gets it, in
// order with the frames that make it stale. It reports whether a conversation
// took it: false when the app provider is not running or the session has no
// bound conversation.
func DeliverPromptSuggestion(sessionKey, text string) bool {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return false
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return false
	}
	b.suggestionSent.Store(true)
	b.send(fap.PromptSuggestion{ConversationID: b.convID, Text: text})
	return true
}

// ClearPromptSuggestion takes down the chip of the conversation bound to
// sessionKey, because the user's input has reached the backend (#2924). The client
// cannot tell a user's turn from a silent background one by its activity, and not
// every user's turn shows it a message first (an ask answered on another device).
// The frame is an empty prompt.suggestion. A conversation that was sent no
// suggestion gets no frame, so an ordinary turn costs nothing. No-op when the app
// provider is not running or the session has no bound conversation.
func ClearPromptSuggestion(sessionKey string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil && b.suggestionSent.Swap(false) {
		b.send(fap.PromptSuggestion{ConversationID: b.convID})
	}
}

// CanShowPromptSuggestion reports whether DeliverPromptSuggestion would reach a
// conversation for sessionKey (#2922). It asks the same question delivery asks,
// not who first claimed the chat (SessionIndex.PlatformForChat): a Telegram chat's
// session adopted by an app conversation stays claimed by Telegram but has a chip.
func CanShowPromptSuggestion(sessionKey string) bool {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	return h != nil && h.bindingForSession(sessionKey) != nil
}

// deliverBoundPrompt sends one durable ExternalPrompt frame to the
// conversation bound to sessionKey — the shared bound-only chit rule behind
// DeliverExternalPrompt and DeliverAgentMessage. No-op when the app provider
// is not running or the session has no bound conversation (a branch, a
// session the app never opened): there is deliberately NO fallback to the
// agent's default chat. Frames sent through the binding are stored and
// replayed on resume, and fall through pushPreview's default case (no push,
// no unread bump — #1279).
// p carries the text and sender fields; its ids are filled in here.
func deliverBoundPrompt(sessionKey string, p fap.ExternalPrompt) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		p.ConversationID, p.MessageID = b.convID, fap.NewULID()
		b.send(p)
	}
}

// DeliverExternalPrompt surfaces a prompt received via the external HTTP /send
// endpoint to the app clients bound to sessionKey, as a durable ExternalPrompt
// frame with no sender fields (foci send has no foci-agent sender). No-op when
// the app provider is not running or the session has no bound conversation
// (foci send to a telegram/discord-only agent surfaces via that transport's
// injected-message header instead).
func DeliverExternalPrompt(sessionKey, text string) {
	deliverBoundPrompt(sessionKey, fap.ExternalPrompt{Text: text})
}

// DeliverAgentMessage surfaces an agent-to-agent message (send_to_session,
// #1286) to the app clients bound to the RECEIVING sessionKey, as a durable
// ExternalPrompt frame whose FromAgent/FromSession name the sending agent and
// session. A message from one of the receiving session's own children (a
// spawn, a depth leaf) also carries the child's name in FromChild, so the app
// labels it "from <child name>" (#2836): the origin's name when it has one
// (send_to_spawner sets it), else tools.ChildName. Same bound-only rule as DeliverExternalPrompt: only the
// conversation bound to sessionKey, no fallback to the agent's default chat,
// no-op when the app provider is not running or the session has no bound
// conversation. App only — Telegram/Discord hear nothing (#1286 ruling).
func DeliverAgentMessage(sessionKey string, origin tools.AgentOrigin) {
	child := origin.ChildName
	if child == "" {
		child = tools.ChildName(sessionKey, origin.SessionKey)
	}
	deliverBoundPrompt(sessionKey, fap.ExternalPrompt{Text: origin.Text, FromAgent: origin.AgentID, FromSession: origin.SessionKey, FromChild: child})
}

// SetWaiting marks the conversation bound to callerSessionKey as waiting on
// another foci agent (targetAgent), or clears it when targetAgent is "". Used by
// send_to_session so the caller's Activity shows "waiting" until its reply turn
// begins (which clears it). No-op when the app provider is not running or the
// session has no live binding.
func SetWaiting(callerSessionKey, targetAgent string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(callerSessionKey); b != nil {
		b.setWaitingDetail(targetAgent)
	}
}

// DurableConnFor returns a connection handle for sessionKey's app registration,
// deliberately bypassing the normal chat-ownership routing (route.ConnFor /
// aggregatingConnMgr's "claimed platform" rule): it's the last-resort durable
// fallback for a session whose usual connection resolution came up empty, not
// the primary delivery path. Every send through the returned connection routes
// through convBinding.send, which persists the frame durably before it even
// checks who's currently connected — so output survives even with nobody live,
// and a client that connects later can replay it. Returns nil (a true nil
// interface, not a typed-nil pointer — see the h.PrimaryBot nil check below) when
// the app provider isn't running or the agent has no app registration at all,
// so callers can cleanly fall through to their own last resort.
func DurableConnFor(sessionKey string) platform.Connection {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return nil
	}
	c := h.BotForSession(sessionKey)
	if c == nil {
		return nil
	}
	return c
}

// ResolvedActivity returns the resolved unified activity (kind + detail) for the
// conversation bound to sessionKey, so /status renders the same value the app
// sees. ok is false when the app provider is not running or the session has no
// app binding (the caller falls back gracefully).
func ResolvedActivity(sessionKey string) (kind, detail string, ok bool) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", "", false
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return "", "", false
	}
	b.mu.Lock()
	k, d := b.resolveActivity()
	b.mu.Unlock()
	return string(k), d, true
}

// NotifyAliasChanged re-advertises the roster to every live socket after a
// conversation alias was set server-side (the agent's set_session_alias tool, a
// backend-generated thread name). The roster is the only frame that carries a
// conversation's title, so without this the app showed the old name until it
// reconnected (#2157). The user's own rename (handleConversationRename) pushes
// for itself. No-op when the app provider isn't running or the chat belongs to
// another platform: aliasForChat reads only the app platform's alias.
func NotifyAliasChanged(agentID, platform string, chatID int64) {
	if platform != "app" {
		return
	}
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.pushRosterAll()
}

// NotifySpeechSourcesChanged re-sends hello to every live socket after the
// server's speech voices may have changed (a live [[tts]] edit, #2945).
// hello.caps.speech is the only frame that carries the voice list, so without
// this the app's Speak ladder showed the old voices until it reconnected. The
// client applies a mid-session hello like the one at connect. No-op when the
// app provider isn't running.
func NotifySpeechSourcesChanged() {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.pushHelloAll()
}

// MintFacetConversation surfaces a facet branch to app clients via the active
// hub (see Hub.mintFacetConversation). Injected into the command layer by the
// gateway as CommandContext.MintFacetConversation. Errors if the app provider
// is not running. A non-empty first.Text is sent into the new conversation as
// the user's first message (#2780).
func MintFacetConversation(agentID, sessionKey string, first command.FirstMessage) (string, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", errors.New("app provider not running")
	}
	return h.mintFacetConversation(agentID, sessionKey, first)
}

// CreateDefaultConversation creates (or reuses) a non-archived app conversation
// for the agent and returns its session key, pushing the new conversation to
// connected devices. Injected into the route Resolver as CreateDefault by the
// delivery paths, fired only when default resolution finds no non-archived
// session. Errors when the app provider is not running or the agent has no app
// connection.
func CreateDefaultConversation(agentID string) (string, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", errors.New("app provider not running")
	}
	return h.createDefaultConversation(agentID)
}

// withHub returns an http.HandlerFunc that resolves the active hub (503 if
// unconfigured) and delegates to fn. Each hub method does its own Bearer auth,
// so no shared middleware is needed.
func withHub(fn func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer abortHandlerOnPanic("http " + r.Method + " " + r.URL.Path)
		activeMu.RLock()
		h := activeHub
		activeMu.RUnlock()
		if h == nil {
			http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
			return
		}
		fn(h, w, r)
	}
}

// WSHandler returns the /app/ws handler (authenticates inside ServeWS).
func WSHandler() http.HandlerFunc { return withHub((*Hub).ServeWS) }

// BlobUploadHandler returns the POST /app/blob handler.
func BlobUploadHandler() http.HandlerFunc { return withHub((*Hub).ServeBlobPost) }

// BlobDownloadHandler returns the GET /app/blob/<id> handler.
func BlobDownloadHandler() http.HandlerFunc { return withHub((*Hub).ServeBlobGet) }

// SkillFileHandler returns the GET /app/skill/<agentId>?path=... handler: the
// current text of a skill markdown file a "Loaded skill" chip names (#2392).
func SkillFileHandler() http.HandlerFunc { return withHub((*Hub).ServeSkillFile) }

// PairHandler returns the POST /app/pair handler (mint a device token).
func PairHandler() http.HandlerFunc { return withHub((*Hub).ServePair) }

// DevicesHandler returns the GET /app/devices handler (list paired devices).
func DevicesHandler() http.HandlerFunc { return withHub((*Hub).ServeDevices) }

// RevokeHandler returns the POST /app/pair/revoke handler (revoke a device).
func RevokeHandler() http.HandlerFunc { return withHub((*Hub).ServeRevoke) }

// PushRegisterHandler returns the POST /app/push/register handler (refresh a
// device's FCM token out-of-band, e.g. after the OS rotates it while offline).
func PushRegisterHandler() http.HandlerFunc { return withHub((*Hub).ServePushRegister) }

// HistoryHandler returns the GET /app/history handler (restart reconciliation).
func HistoryHandler() http.HandlerFunc { return withHub((*Hub).ServeHistory) }

// ReplayHandler returns the GET /app/replay handler (durable content backfill).
func ReplayHandler() http.HandlerFunc { return withHub((*Hub).ServeReplay) }

// STTHandler returns the GET /app/stt handler (direct-call STT credential).
func STTHandler() http.HandlerFunc { return withHub((*Hub).ServeSTT) }

// SpeechHandler returns the POST /app/speech handler (read a message aloud, #2858).
func SpeechHandler() http.HandlerFunc { return withHub((*Hub).ServeSpeech) }

// AvatarHandler returns the GET /app/avatar/<agentId> handler (agent avatar image).
func AvatarHandler() http.HandlerFunc { return withHub((*Hub).ServeAvatar) }

// ShellRunnableHandler returns the POST /app/shell/runnable handler (#2878).
func ShellRunnableHandler() http.HandlerFunc { return withHub((*Hub).ServeShellRunnable) }

// MermaidHandler returns the POST /app/render/mermaid handler (diagram → PNG, #1980).
func MermaidHandler() http.HandlerFunc { return withHub((*Hub).ServeMermaid) }
