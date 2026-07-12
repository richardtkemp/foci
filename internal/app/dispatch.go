package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
)

// sendRaw queues a socket-level (non-conversation-scoped) server frame, such as
// the server hello or a pong. These carry no per-conversation seq.
func (c *wsClient) sendRaw(frame fap.ServerFrame) {
	wire, err := fap.Encode(frame, 0, 0, "", "")
	if err != nil {
		log.Errorf("app", "encode %s: %v", frame.Type(), err)
		return
	}
	c.enqueue(wire)
}

// dispatchInbound decodes one inbound text frame and routes it. Unknown frame
// types decode to a nil Frame and are ignored (forward-compat). Malformed
// frames are logged and dropped — one bad frame must not kill the socket.
func (h *Hub) dispatchInbound(client *wsClient, data []byte) {
	in, err := fap.Decode(string(data))
	if err != nil {
		log.Warnf("app", "drop malformed inbound frame: %v", err)
		return
	}

	// Reliability gate (§3): for conversation-scoped frames whose durable state
	// already exists, dedup by (conversationId, envelope id) — dropping a resent
	// outbox entry after reconnect — and fold the piggybacked ack to trim our
	// replay buffer. The first message of a brand-new conversation has no state
	// yet; its binding is created downstream in routeUserText.
	if convID := inboundConvID(in.Frame); convID != "" {
		if b := h.convForReliability(convID); b != nil {
			if !b.acceptInbound(client, in.ID, in.Seq) {
				return
			}
			b.ackInbound(client, in.Ack)
		}
	}

	switch f := in.Frame.(type) {
	case fap.ClientHello:
		client.mu.Lock()
		client.deviceID = f.Client.DeviceID
		client.features = featureSet(f.Features)
		client.mu.Unlock()
		// A master-key socket learns its deviceId here; evict any older socket for
		// the same device (wire §9, close 4409) so a reconnecting phone never ends
		// up with two live sockets on one conversation.
		h.evictOtherDeviceSockets(client, f.Client.DeviceID)
		// Register the device's FCM token for offline wake pushes.
		h.tokens.set(f.Client.DeviceID, f.PushToken)
		h.pushRoster(client)
		h.pushSettings(client)
		h.pushReads(client)
		h.pushDrafts(client)
		// Reconnect resume: re-attach (which recomputes the capability union across
		// attached clients) + replay each conversation the client still has
		// unrendered frames for.
		h.resumeConversations(client, f.Resume)
		// Persist the capability UNION (not just this client's caps) per resumed
		// conv, so a restart that rebuilds bindings caps-less still resolves checks
		// against every device that was attached.
		if idx := h.deps.SessionIndex; idx != nil {
			for _, rp := range f.Resume {
				h.mu.RLock()
				b := h.convs[rp.ConversationID]
				h.mu.RUnlock()
				if b != nil {
					_ = idx.SetChatMetadata(b.agentID, "app", b.chatID, "features", b.featuresCSV())
				}
			}
		}
		// Seed the open-set from the resume points' open flags.
		open := make(map[string]struct{})
		for _, rp := range f.Resume {
			if rp.Open {
				open[rp.ConversationID] = struct{}{}
			}
		}
		client.mu.Lock()
		client.openConvIDs = open
		client.mu.Unlock()

	case fap.ConversationOpen:
		h.handleConversationOpen(client, f)

	case fap.ConversationOpenSet:
		open := make(map[string]struct{}, len(f.ConversationIDs))
		for _, id := range f.ConversationIDs {
			open[id] = struct{}{}
		}
		client.mu.Lock()
		client.openConvIDs = open
		client.mu.Unlock()

	case fap.ConversationList:
		h.pushRoster(client)

	case fap.ConversationRename:
		h.handleConversationRename(client, f)

	case fap.ConversationSetDefault:
		h.handleConversationSetDefault(client, f)

	case fap.ConversationArchive:
		h.handleConversationArchive(client, f)

	case fap.SettingPut:
		h.handleSettingPut(f)

	case fap.DraftPut:
		h.handleDraft(client, f)
	case fap.ConfigGet:
		h.handleConfigGet(client)

	case fap.ConfigPut:
		h.handleConfigPut(client, f)

	case fap.ConfigUnset:
		h.handleConfigUnset(client, f)

	case fap.ClientMessage:
		h.routeUserTurn(client, f.ConversationID, f.AgentID, f.Text, h.resolveAttachments(f.Attachments), in.ID, in.Seq, steerPreference(f.Steer), f.TranscribeOnly)

	case fap.InteractiveResponse:
		h.handleInteractiveResponse(client, f)

	case fap.InteractiveProgress:
		h.handleInteractiveProgress(f)

	case fap.WizardResponse:
		// Off the socket goroutine: HandleMessage runs wizard logic that may
		// block (e.g. config/secrets writes), like command dispatch.
		safeGo("wizard-response", func() { h.handleWizardResponse(f) })

	case fap.Command:
		h.routeCommand(client, f)

	case fap.Ping:
		client.sendRaw(fap.Pong{})

	case fap.ClientTyping:
		// No upstream surface.

	case fap.Read:
		h.handleRead(client, f)

	case fap.ToolResult:
		// Not conversation-scoped — no reliability gating above. Hand straight
		// to the registry, which routes by InvocationID to the waiting
		// InvokeTool caller (if any; late/unsolicited results are dropped).
		h.deliverToolResult(f)

	default:
		// nil Frame (unknown t) — forward-compat, ignore.
	}
}

// handleInteractiveResponse routes a button tap back into foci's interactive
// machinery. f.Data is foci's pre-encoded "<promptID>:<index>" token (the app
// echoes Choice.Data verbatim), so it goes straight to HandleInteractiveCallback,
// which fires the registered callback (allow / deny / answer → CC) and returns
// the resolution edit text.
//
// Ordering guard: a tap that advances a multi-question ask re-presents the next
// question — a fresh `interactive` frame on the same binding, re-registering the
// same promptID — *synchronously* inside HandleInteractiveCallback. Emitting the
// "✅ <label>" edit (or deleting the registration) afterward would clobber that
// new question. So if the binding's seq advanced during the callback, we leave
// the prompt and its registration untouched.
func (h *Hub) handleInteractiveResponse(client *wsClient, f fap.InteractiveResponse) {
	// Batched (multi-question) reply. Routed by the batchPrompt registry, not by
	// "Answers non-empty", so a streamed completion — whose answers already
	// arrived as InteractiveProgress, leaving Answers empty — still resolves here.
	// Carried Answers reconcile the accumulated set (covering a dropped progress
	// frame). A Done edit fans out so a second client viewing the form closes it.
	if bp, ok := h.batchPromptByID(f.PromptID); ok {
		h.deleteBatchPrompt(f.PromptID)
		bp.mu.Lock()
		for i, a := range f.Answers {
			if i < len(bp.answers) && a != "" {
				bp.answers[i] = a
			}
		}
		final := append([]string(nil), bp.answers...)
		bp.mu.Unlock()
		bp.b.send(fap.InteractiveProgressEdit{ConversationID: f.ConversationID, PromptID: f.PromptID, Answers: final, Done: true})
		bp.onResp(final)
		return
	}

	client.mu.Lock()
	b := client.convByID[f.ConversationID]
	client.mu.Unlock()

	var seqBefore int64
	if b != nil {
		seqBefore = b.currentSeq()
	}

	edit, _, ok := platform.HandleInteractiveCallback(f.Data)
	if !ok {
		// Unknown / expired / already-resolved prompt: no callback fired, so no
		// re-registration could have happened — safe to drop any stale entry.
		h.deletePrompt(f.PromptID)
		return
	}
	if b != nil && b.currentSeq() != seqBefore {
		// A follow-up question re-rendered + re-registered this promptID; keep both.
		return
	}
	h.deletePrompt(f.PromptID)
	if edit != "" && b != nil {
		b.send(fap.InteractiveEdit{ConversationID: f.ConversationID, PromptID: f.PromptID, Text: edit})
	}
}

// handleInteractiveProgress records one answered question of a batched ask and
// mirrors the accumulated answers to the other attached clients, so a form can
// be part-answered on one device and finished on another.
func (h *Hub) handleInteractiveProgress(f fap.InteractiveProgress) {
	bp, ok := h.batchPromptByID(f.PromptID)
	if !ok {
		return
	}
	bp.mu.Lock()
	if f.Index < 0 || f.Index >= len(bp.answers) {
		bp.mu.Unlock()
		return
	}
	bp.answers[f.Index] = f.Answer
	snapshot := append([]string(nil), bp.answers...)
	bp.mu.Unlock()
	bp.b.send(fap.InteractiveProgressEdit{ConversationID: f.ConversationID, PromptID: f.PromptID, Answers: snapshot})
}

// handleConversationOpen creates a server-assigned conversation for an agent
// (the server owns conversationId; the app learns it from the roster reply). A
// supplied sessionKey resumes a specific named/independent session for it.
func (h *Hub) handleConversationOpen(client *wsClient, f fap.ConversationOpen) {
	agentID := f.AgentID
	if agentID == "" || h.PrimaryBot(agentID) == nil {
		agentID = h.defaultAgentID()
	}
	if h.PrimaryBot(agentID) == nil {
		client.sendRaw(fap.ErrorFrame{Code: "no_agent", Message: "no agent available"})
		return
	}

	// Reopening a named session that already has a durable conversation must
	// reuse it, not mint a duplicate that races the existing binding in
	// bySession. Only a fresh open (no session key, or an unknown one) creates a
	// new conversation. Legacy (pre-stable-identity) keys from an older client
	// DB are normalised first so the reopen finds the migrated conversation.
	if f.SessionKey != "" {
		if stable, isLegacy := session.LegacyKeyToStable(f.SessionKey); isLegacy {
			f.SessionKey = stable
		}
		if existing := h.bindingForSession(f.SessionKey); existing != nil {
			existing.attach(client)
			h.pushRoster(client)
			return
		}
	}

	// Adopt a client-assigned id when supplied (#new-conv-instant) so the app can
	// create + open the conversation locally without waiting to learn the id;
	// otherwise mint one. ensureBinding is idempotent, so a reopen of an id that
	// already has a binding (e.g. after the first message already created it)
	// reuses it rather than minting a duplicate.
	convID := f.ConversationID
	if convID == "" {
		convID = fap.NewULID()
	}
	b := h.ensureBinding(client, agentID, convID)
	if f.SessionKey != "" {
		h.adoptSession(b, f.SessionKey)
	}
	// Advertise the new conversation (+ its session key) via an updated roster;
	// the app upserts it from the hello frame.
	h.pushRoster(client)
}

// handleConversationRename sets (or clears, when Title is empty) the user-friendly
// alias for a conversation. The alias persists in the session index's chat_metadata
// keyed by the stable app chatID — so it survives session-key rotation and restarts —
// and the updated roster is pushed back to this socket (other devices refresh on their
// next hello/conversation.list).
func (h *Hub) handleConversationRename(client *wsClient, f fap.ConversationRename) {
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		if err := idx.SetChatAliasUnique(b.agentID, "app", b.chatID, f.Title); err != nil {
			log.Warnf("app", "rename %s: persist alias: %v", f.ConversationID, err)
			code, msg := "rename_failed", "Couldn't save that name."
			if errors.Is(err, session.ErrAliasTaken) {
				code, msg = "alias_taken", "That name is already used by another chat."
			}
			client.sendRaw(fap.ErrorFrame{ConversationID: f.ConversationID, Code: code, Message: msg, Retryable: true})
			return
		}
	}
	h.pushRoster(client)
}

// handleConversationSetDefault sets (f.IsDefault) or clears this conversation as
// the agent's default chat for the app platform. The default persists in the
// session index keyed by the stable app chatID — surviving session-key rotation
// and restarts — and is used by keepalive/cron routing. The updated roster (with
// ConversationInfo.IsDefault) is pushed back to this socket; other devices
// refresh on their next hello/conversation.list. Setting a new default clears any
// previous one for the platform (SetDefaultChat deletes-then-sets). Mirrors
// handleConversationRename (the established per-chat-metadata round-trip).
func (h *Hub) handleConversationSetDefault(client *wsClient, f fap.ConversationSetDefault) {
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		var err error
		if f.IsDefault {
			err = idx.SetDefaultChat(b.agentID, "app", b.chatID)
		} else {
			err = idx.ClearDefaultChat(b.agentID, "app")
		}
		if err != nil {
			log.Warnf("app", "setDefault %s (isDefault=%v): %v", f.ConversationID, f.IsDefault, err)
		}
	}
	h.pushRoster(client)
}

// handleConversationArchive sets or clears the archived flag on a conversation.
// Reversible — unlike the old destructive archive, this does NOT purge replay
// frames, drop the binding, flip session status, or fire a final reflection.
// It persists only the is_archived flag (keyed by agent+platform+chatID in
// chat_metadata, alongside is_default) and pushes back an updated roster so the
// app and every other device reconciles. The binding stays live: inbound frames
// still flow and history is retained, so unarchive (Archived=false) restores the
// full thread. Archiving the agent's default chat is refused with an ErrorFrame
// (the client's Archive control is disabled for the default, so this is a
// belt-and-braces guard for other devices/older clients): a default that could
// be archived would silently degrade session-blind delivery. Mirrors
// handleConversationSetDefault (the established per-chat-metadata round-trip).
// (#app-archive-flag)
func (h *Hub) handleConversationArchive(client *wsClient, f fap.ConversationArchive) {
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		log.Debugf("app", "archive %s: no live binding (flag not set)", f.ConversationID)
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		if f.Archived && idx.DefaultChatForAgent(b.agentID, "app") == b.chatID {
			log.Infof("app", "refused archive of default conversation %s (agent %s)", f.ConversationID, b.agentID)
			client.sendRaw(fap.ErrorFrame{ConversationID: f.ConversationID, Code: "archive_default", Message: "This is the default chat. Set another default before archiving it."})
			h.pushRoster(client) // reverts the client's optimistic archived flag
			return
		}
		if err := idx.SetArchivedChat(b.agentID, "app", b.chatID, f.Archived); err != nil {
			log.Warnf("app", "archive %s (archived=%v): %v", f.ConversationID, f.Archived, err)
		}
	}
	log.Infof("app", "archived=%v conversation %s (session %s, frames retained)", f.Archived, f.ConversationID, b.sessionKey)
	h.pushRoster(client)
}

// handleSettingPut persists one synced app-preference to the global bag and
// re-broadcasts the merged snapshot to every settings-capable client (including
// the sender, whose local guard drops the echo).
func (h *Hub) handleSettingPut(f fap.SettingPut) {
	if f.Key == "" {
		return
	}
	h.broadcastSettings(h.storeAppSetting(f.Key, f.Value))
}

// handleRead persists a conversation's read watermark and mirrors it to the
// user's other devices. The reliability gate already consumed the frame's ack;
// this adds the cross-device half.
func (h *Hub) handleRead(client *wsClient, f fap.Read) {
	if f.MessageID == "" {
		return
	}
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		_ = idx.SetChatMetadata(b.agentID, "app", b.chatID, "last_read", f.MessageID)
	}
	h.broadcastReadExcept(f.ConversationID, f.MessageID, client)
}

// handleDraft persists a conversation's unsent composer text and mirrors it to
// the user's other devices. Empty Text is a valid clear (composer emptied /
// message sent) — unlike handleRead, there is no non-empty guard. Fire-and-
// forget: DraftPut is not conversation-reliability-scoped (see inboundConvID),
// so there is no ack to fold here.
func (h *Hub) handleDraft(client *wsClient, f fap.DraftPut) {
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		_ = idx.SetChatMetadata(b.agentID, "app", b.chatID, "draft", f.Text)
	}
	h.broadcastDraftExcept(f.ConversationID, f.Text, client)
}

// routeCommand dispatches a slash command through the agent's command registry,
// returning its response as message frame(s). Dispatch runs off the read pump
// because some commands do real work.
func (h *Hub) routeCommand(client *wsClient, f fap.Command) {
	if f.ConversationID == "" || f.Name == "" {
		return
	}
	client.mu.Lock()
	deviceID := client.deviceID
	client.mu.Unlock()
	// Agent comes from the frame (the conversation's owner), not a socket-wide
	// focus; bind first so b.agentID stays authoritative for a warm conversation.
	agentID := f.AgentID
	if agentID == "" {
		agentID = h.defaultAgentID()
	}
	h.mu.RLock()
	existed := h.convs[f.ConversationID] != nil
	h.mu.RUnlock()
	b := h.ensureBinding(client, agentID, f.ConversationID)
	if existed && f.AgentID != "" && f.AgentID != b.agentID {
		log.Warnf("app", "frame agentId %q disagrees with conversation owner %q (conv=%q) — routing to owner", f.AgentID, b.agentID, f.ConversationID)
	}
	conn := h.PrimaryBot(b.agentID)
	if conn == nil || conn.commands == nil {
		client.sendRaw(fap.ErrorFrame{ConversationID: f.ConversationID, Code: "no_commands", Message: "commands unavailable"})
		return
	}
	// A command is user activity too — record interaction so the periodic
	// runner (reflection/consolidation/reset-idle-guard) sees the user as active
	// even when the turn is a slash-command rather than an enqueued message.
	if conn.OnUserMessage != nil {
		conn.OnUserMessage()
	}
	req := command.Request{
		Name:       strings.ToLower(strings.TrimPrefix(f.Name, "/")),
		Args:       f.Args,
		SessionKey: b.sessionKey,
		UserID:     deviceID,
		ChatID:     b.chatID,
	}
	safeGo("dispatch-command", func() { h.dispatchCommand(conn, b, req) })
}

func (h *Hub) dispatchCommand(conn *appConn, b *convBinding, req command.Request) {
	ctx := h.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Inject the session key into ctx so commands that read it via
	// tools.SessionKeyFromContext (e.g. /stop, /reset, /plan) work in the
	// app path. Telegram's Dispatcher does this at dispatchRequest; the app
	// path bypasses the queue and calls Dispatch directly. Without this,
	// /stop from the app UI returns "no active session". See TODO #88.
	ctx = tools.WithSessionKey(ctx, req.SessionKey)

	// Echo the user's command back as a user-role message BEFORE dispatch.
	// Telegram's client renders outgoing text natively; the app relies on
	// server-pushed frames, so without this echo the user sees only the
	// system response (e.g. "📋 Planning…") with no indication of what they
	// sent. Mirrors Telegram's "your command always appears" behaviour.
	// Emitted before dispatch so it shows regardless of outcome (success,
	// error, unknown) — same as a typed /command on Telegram.
	userText := "/" + req.Name
	if req.Args != "" {
		userText += " " + req.Args
	}
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "user", Text: userText})

	// Snapshot the session's wizard generation so maybeStartWizard can tell
	// whether this dispatch activated a (new) wizard for it.
	wizardGenBefore := conn.commands.WizardGen(req.SessionKey)

	resp, handled, err := conn.commands.Dispatch(ctx, req, conn.cmdCtx)
	switch {
	case err != nil:
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: "command error: " + err.Error()})
		return
	case !handled:
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: "unknown command: /" + req.Name})
		return
	}
	// A command that activated a wizard delivers its prompt as a structured
	// wizard.step frame (capable clients only) — the plain-text render below
	// would duplicate it.
	if !h.maybeStartWizard(conn, b, resp, userText, req.SessionKey, wizardGenBefore) {
		for _, part := range commandParts(resp) {
			b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: part})
		}
	}
	if resp.DocPath != "" {
		_ = conn.SendDocumentToChat(b.chatID, resp.DocPath, "")
	}
}

// commandParts returns the message parts of a command response (multi-part when
// Parts is set, else the single Text).
func commandParts(resp command.Response) []string {
	if len(resp.Parts) > 0 {
		return resp.Parts
	}
	if strings.TrimSpace(resp.Text) != "" {
		return []string{resp.Text}
	}
	return nil
}

// inboundConvID returns the conversationId a client frame is scoped to, or ""
// for socket-level frames (hello / conversation.open / list / ping) that carry
// no conversation and bypass the reliability gate.
func inboundConvID(frame any) string {
	switch f := frame.(type) {
	case fap.ClientMessage:
		return f.ConversationID
	case fap.InteractiveResponse:
		return f.ConversationID
	case fap.WizardResponse:
		return f.ConversationID
	case fap.Command:
		return f.ConversationID
	case fap.ClientTyping:
		return f.ConversationID
	case fap.Read:
		return f.ConversationID
	case fap.ConversationRename:
		return f.ConversationID
	case fap.ConversationSetDefault:
		return f.ConversationID
	case fap.ConversationArchive:
		return f.ConversationID
	default:
		return ""
	}
}

// routeUserTurn resolves the agent + conversation binding and enqueues a user
// turn. A turn with neither text nor attachments, or no conversation, is ignored.
//
// inID/inSeq are the inbound frame's envelope id/seq. The reliability gate in
// dispatchInbound only dedups frames whose binding already exists; the first
// message of a brand-new conversation creates its binding *here*, so the gate
// skipped it. We seed the freshly created binding's dedup set below so a copy
// replayed from the client outbox after a reconnect is dropped (the image
// double-send bug). A warm binding was already recorded by the gate, so we
// only seed when we just created it.
func (h *Hub) routeUserTurn(client *wsClient, convID, agentID, text string, atts []platform.Attachment, inID string, inSeq int64, steer agent.SteerPreference, transcribeOnly bool) {
	if convID == "" || (text == "" && len(atts) == 0) {
		return
	}

	// Command interception: if the text is a routable command (/cmd or .cmd),
	// divert to the command dispatch path instead of enqueuing an agent turn.
	// This mirrors Telegram/Discord's interception pipeline — without it,
	// "/misc pprof on" would start a turn (firing a typing indicator) and be
	// sent to the LLM as a user message rather than executed as a command.
	// IsRoutableCommand already guards against file paths (/home/foci/x has an
	// embedded slash in its first token → not a command) and only routes .cmd
	// when the command is actually registered (so ".sigh" reaches the agent).
	if text != "" && (text[0] == '/' || text[0] == '.') {
		aid := agentID
		if aid == "" {
			aid = h.defaultAgentID()
		}
		if conn := h.PrimaryBot(aid); conn != nil && conn.commands != nil {
			if dispatch.IsRoutableCommand(text, conn.commands) {
				name, args, _ := strings.Cut(text[1:], " ")
				h.routeCommand(client, fap.Command{
					ConversationID: convID,
					AgentID:        agentID,
					Name:           name,
					Args:           args,
				})
				return
			}
		}
	}

	// Apply message transforms (mirrors the Telegram/Discord interceptor).
	// A transform can rewrite the text or turn it into a command (e.g. "st" → "/stop").
	if text != "" {
		aid := agentID
		if aid == "" {
			aid = h.defaultAgentID()
		}
		if conn := h.PrimaryBot(aid); conn != nil && conn.agentRef != nil {
			if transformed := conn.agentRef.TransformMessage(text); transformed != text {
				text = transformed
				// A transform may have produced a command — try dispatching it.
				if text != "" && (text[0] == '/' || text[0] == '.') && conn.commands != nil {
					if dispatch.IsRoutableCommand(text, conn.commands) {
						name, args, _ := strings.Cut(text[1:], " ")
						h.routeCommand(client, fap.Command{
							ConversationID: convID,
							AgentID:        agentID,
							Name:           name,
							Args:           args,
						})
						return
					}
				}
			}
		}
	}

	client.mu.Lock()
	deviceID := client.deviceID
	client.mu.Unlock()

	// The conversation determines its agent. A warm binding records the owning
	// agent (fixed at creation) and its session key is prefixed with it; the
	// frame's agentId is authoritative only to seed a cold binding (e.g. after a
	// server restart evicted it from memory). Either way we route to b.agentID —
	// never a socket-wide "current agent", which on a multi-agent socket could
	// disagree with the conversation, enqueue to the wrong agent carrying this
	// conversation's session key, and get the turn silently dropped by the
	// ownership invariant (#906/#907). agentId empty (legacy/forward-compat) only
	// seeds the default agent when minting a brand-new conversation.
	frameAgent := agentID
	if agentID == "" {
		agentID = h.defaultAgentID()
	}
	h.mu.RLock()
	existed := h.convs[convID] != nil
	h.mu.RUnlock()
	b := h.ensureBinding(client, agentID, convID)

	// Tripwire: a well-behaved client derives agentId from the conversation row,
	// so it can never disagree with the owning binding. If it does, a client has
	// regressed — route to the owner (b.agentID, authoritative) but make the
	// mismatch loud so we catch it instead of silently correcting (#906/#907).
	if existed && frameAgent != "" && frameAgent != b.agentID {
		log.Warnf("app", "frame agentId %q disagrees with conversation owner %q (conv=%q) — routing to owner", frameAgent, b.agentID, convID)
	}

	conn := h.PrimaryBot(b.agentID)
	if conn == nil {
		log.Warnf("app", "no agent for inbound message (agent=%q conv=%q)", b.agentID, convID)
		client.sendRaw(fap.ErrorFrame{
			ConversationID: convID,
			Code:           "no_agent",
			Message:        "no agent is available for this conversation",
		})
		return
	}

	if !existed {
		// First message on a cold binding: record its envelope id so the gate
		// drops the replayed copy after a reconnect.
		b.acceptInbound(client, inID, inSeq)
	}
	text, atts = h.transcribeVoice(conn, text, atts)
	if transcribeOnly {
		// Return the transcript for the user to edit; don't echo or run a turn.
		b.send(fap.Transcript{ConversationID: convID, Text: text})
		return
	}
	if text == "" && len(atts) == 0 {
		return // voice transcription yielded nothing and there's no other content
	}
	// Echo the user's message back as a durable user-role frame so it persists to
	// the replay store and reaches other devices. The sending device reconciles it
	// against its optimistic copy by MessageID (the inbound envelope id), so it
	// doesn't double-render. Without this the message lives only on the sender; a
	// freshly-paired device (which rebuilds from replayed server frames) never sees
	// it — only agent/system messages, which already flow as server frames.
	if text != "" {
		b.send(fap.ServerMessage{ConversationID: convID, MessageID: inID, Role: "user", Text: text})
	}
	// User message confirmed bound for the agent — record interaction. This is
	// the app transport's only lastInteraction signal (the periodic runner gates
	// reflection/consolidation/reset-idle-guard on it); without it those timers
	// see "idle since boot" forever and never fire on app-driven agents.
	if conn.OnUserMessage != nil {
		conn.OnUserMessage()
	}
	conn.agentRef.Enqueue(agent.Envelope{
		SessionKey:  b.sessionKey,
		Text:        text,
		Attachments: atts,
		UserID:      deviceID,
		ChatID:      b.chatID,
		ReceivedAt:  time.Now(),
		Original:    convID,
		Driver:      conn,
		Steer:       steer,
	})
}

// steerPreference maps the wire-level steer field ("steer" / "queue" / "")
// to the agent's per-envelope preference. Unknown values degrade to the
// config default rather than erroring — forward compatibility with newer
// clients.
func steerPreference(s string) agent.SteerPreference {
	switch s {
	case "steer":
		return agent.SteerAlways
	case "queue":
		return agent.SteerNever
	default:
		return agent.SteerDefault
	}
}

// transcribeVoice replaces voice attachments with their STT transcript, merged
// into the turn text. Non-voice attachments pass through. With no transcriber,
// or on a transcription error, the voice attachment is kept as-is (the agent can
// still see the audio bytes). Mirrors telegram's inbound voice-note handling.
func (h *Hub) transcribeVoice(conn *appConn, text string, atts []platform.Attachment) (string, []platform.Attachment) {
	if conn.stt == nil || len(atts) == 0 {
		return text, atts
	}
	ctx := h.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	kept := make([]platform.Attachment, 0, len(atts))
	var transcripts []string
	for _, a := range atts {
		if a.Type != fap.MediaVoice || len(a.Data) == 0 {
			kept = append(kept, a)
			continue
		}
		tr, err := conn.stt.Transcribe(ctx, a.Data, voiceFilename(a))
		if err != nil {
			log.Warnf("app", "voice transcribe: %v", err)
			kept = append(kept, a) // fall back to passing the audio through
			continue
		}
		if tr = strings.TrimSpace(tr); tr != "" {
			transcripts = append(transcripts, tr)
		}
	}
	if len(transcripts) > 0 {
		joined := strings.Join(transcripts, " ")
		if text == "" {
			text = joined
		} else {
			text += "\n" + joined
		}
	}
	return text, kept
}

// voiceFilename derives a filename (the STT backend may use its extension for
// format detection) from a voice attachment's MIME type.
func voiceFilename(a platform.Attachment) string {
	switch {
	case strings.Contains(a.MimeType, "ogg"), strings.Contains(a.MimeType, "opus"):
		return "voice.ogg"
	case strings.Contains(a.MimeType, "mp4"), strings.Contains(a.MimeType, "m4a"), strings.Contains(a.MimeType, "aac"):
		return "voice.m4a"
	case strings.Contains(a.MimeType, "mpeg"), strings.Contains(a.MimeType, "mp3"):
		return "voice.mp3"
	case strings.Contains(a.MimeType, "wav"):
		return "voice.wav"
	default:
		return "voice.ogg"
	}
}

// resolveAttachments turns a message's blob references into platform
// attachments, reading each blob from the store. Small blobs are loaded into
// Data (for in-memory consumers like vision); SavedPath always points at the
// on-disk blob. Unknown/unreadable blobs are skipped with a warning.
func (h *Hub) resolveAttachments(refs []fap.AttachmentRef) []platform.Attachment {
	if len(refs) == 0 {
		return nil
	}
	out := make([]platform.Attachment, 0, len(refs))
	for _, r := range refs {
		meta, ok := h.blobs.get(r.BlobID)
		if !ok {
			log.Warnf("app", "inbound attachment blob %q not found", r.BlobID)
			continue
		}
		var data []byte
		if meta.size <= inlineAttachmentMax {
			if d, err := os.ReadFile(meta.path); err == nil {
				data = d
			} else {
				log.Warnf("app", "read attachment blob %q: %v", r.BlobID, err)
			}
		}
		out = append(out, platform.Attachment{
			Type:      firstNonEmpty(r.Kind, meta.kind),
			Data:      data,
			MimeType:  firstNonEmpty(r.MIME, meta.mime),
			SavedPath: meta.path,
		})
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
