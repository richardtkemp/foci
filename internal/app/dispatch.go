package app

import (
	"context"
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
			if !b.acceptInbound(in.ID, in.Seq) {
				return
			}
			b.ackInbound(in.Ack)
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
		client.sendRaw(fap.HelloServer{
			Version: fap.ProtocolVersion,
			Caps:    h.caps(),
			Agents:  h.agentRoster(),
		})
		// Reconnect resume: re-attach + replay each conversation the client still
		// has unrendered frames for.
		h.resumeConversations(client, f.Resume)

	case fap.ConversationOpen:
		h.handleConversationOpen(client, f)

	case fap.ConversationList:
		client.sendRaw(fap.HelloServer{
			Version: fap.ProtocolVersion,
			Caps:    h.caps(),
			Agents:  h.agentRoster(),
		})

	case fap.ConversationRename:
		h.handleConversationRename(client, f)

	case fap.ConversationSetDefault:
		h.handleConversationSetDefault(client, f)

	case fap.ConversationArchive:
		h.handleConversationArchive(client, f)

	case fap.ClientMessage:
		h.routeUserTurn(client, f.ConversationID, f.AgentID, f.Text, h.resolveAttachments(f.Attachments), in.ID, in.Seq)

	case fap.InteractiveResponse:
		h.handleInteractiveResponse(client, f)

	case fap.Command:
		h.routeCommand(client, f)

	case fap.Ping:
		client.sendRaw(fap.Pong{})

	case fap.ClientTyping, fap.Read:
		// typing: no upstream surface; read: ack/unread handled by the
		// reliability gate above.

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
	// Batched (multi-question) reply: the app returns every answer at once in
	// Answers. Route it to the batched-ask callback and skip the single-prompt
	// machinery (no per-question edit; the app resolves its own form on submit).
	if len(f.Answers) > 0 {
		log.Debugf("app", "InteractiveResponse(batched): conv=%s prompt=%s answers=%v",
			f.ConversationID, f.PromptID, f.Answers)
		if bp, ok := h.batchPromptByID(f.PromptID); ok {
			h.deleteBatchPrompt(f.PromptID)
			bp.onResp(f.Answers)
		}
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
	// new conversation.
	if f.SessionKey != "" {
		if existing := h.bindingForSession(f.SessionKey); existing != nil {
			existing.attach(client)
			client.sendRaw(fap.HelloServer{
				Version: fap.ProtocolVersion,
				Caps:    h.caps(),
				Agents:  h.agentRoster(),
			})
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
	client.sendRaw(fap.HelloServer{
		Version: fap.ProtocolVersion,
		Caps:    h.caps(),
		Agents:  h.agentRoster(),
	})
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
		if err := idx.SetChatMetadata(b.agentID, "app", b.chatID, "alias", strings.TrimSpace(f.Title)); err != nil {
			log.Warnf("app", "rename %s: persist alias: %v", f.ConversationID, err)
		}
	}
	client.sendRaw(fap.HelloServer{
		Version: fap.ProtocolVersion,
		Caps:    h.caps(),
		Agents:  h.agentRoster(),
	})
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
	client.sendRaw(fap.HelloServer{
		Version: fap.ProtocolVersion,
		Caps:    h.caps(),
		Agents:  h.agentRoster(),
	})
}

// handleConversationArchive archives a conversation: it purges the conversation's
// durable replay frames (excluding it from the startup binding restore — presence
// of frames IS the restore signal, so there is no archived flag to store), drops
// its live binding, flips the session status to archived (which also removes it
// from periodic reflection, whose query filters status='active'), and fires one
// FINAL reflection if the session is due (reusing the same "activity since last
// reflection" gate). One-directional: there is no server-side unarchive — the
// app's unarchive is a local re-show, and the binding is recreated lazily on its
// next use. (#app-binding-restore)
func (h *Hub) handleConversationArchive(_ *wsClient, f fap.ConversationArchive) {
	h.mu.Lock()
	b := h.convs[f.ConversationID]
	if b != nil {
		delete(h.convs, f.ConversationID)
		delete(h.bySession, b.sessionKey)
	}
	h.mu.Unlock()

	purged := h.frames.PurgeConv(f.ConversationID)
	if b == nil {
		log.Debugf("app", "archive %s: no live binding (frames purged=%d)", f.ConversationID, purged)
		return
	}
	// Final reflection BEFORE flipping status — gated by reflection's own
	// "activity since last reflection" rule, so an already-reflected session with
	// no new input is skipped. nil when no reflector is wired (tests, no periodic).
	if h.reflectOnArchive != nil {
		h.reflectOnArchive(b.sessionKey)
	}
	// Flip status last so the session stops being picked up by periodic reflection
	// (its query filters status='active').
	if idx := h.deps.SessionIndex; idx != nil {
		idx.UpdateStatus(b.sessionKey, session.SessionStatusArchived)
	}
	log.Infof("app", "archived conversation %s (session %s, frames purged=%d)", f.ConversationID, b.sessionKey, purged)
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

	resp, handled, err := conn.commands.Dispatch(ctx, req, conn.cmdCtx)
	switch {
	case err != nil:
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: "command error: " + err.Error()})
		return
	case !handled:
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: "unknown command: /" + req.Name})
		return
	}
	for _, part := range commandParts(resp) {
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "system", Text: part})
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
func (h *Hub) routeUserTurn(client *wsClient, convID, agentID, text string, atts []platform.Attachment, inID string, inSeq int64) {
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
		b.acceptInbound(inID, inSeq)
	}
	text, atts = h.transcribeVoice(conn, text, atts)
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
	})
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
