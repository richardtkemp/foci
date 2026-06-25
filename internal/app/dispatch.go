package app

import (
	"context"
	"os"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/log"
	"foci/internal/platform"
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

	case fap.ClientMessage:
		h.routeUserTurn(client, f.ConversationID, f.Text, h.resolveAttachments(f.Attachments))

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
	client.mu.Lock()
	client.agentID = agentID
	client.mu.Unlock()

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

	convID := fap.NewULID()
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

// routeCommand dispatches a slash command through the agent's command registry,
// returning its response as message frame(s). Dispatch runs off the read pump
// because some commands do real work.
func (h *Hub) routeCommand(client *wsClient, f fap.Command) {
	if f.ConversationID == "" || f.Name == "" {
		return
	}
	client.mu.Lock()
	agentID := client.agentID
	deviceID := client.deviceID
	client.mu.Unlock()
	if agentID == "" {
		agentID = h.defaultAgentID()
	}
	conn := h.PrimaryBot(agentID)
	if conn == nil || conn.commands == nil {
		client.sendRaw(fap.ErrorFrame{ConversationID: f.ConversationID, Code: "no_commands", Message: "commands unavailable"})
		return
	}
	b := h.ensureBinding(client, agentID, f.ConversationID)
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
	default:
		return ""
	}
}

// routeUserTurn resolves the agent + conversation binding and enqueues a user
// turn. A turn with neither text nor attachments, or no conversation, is ignored.
func (h *Hub) routeUserTurn(client *wsClient, convID, text string, atts []platform.Attachment) {
	if convID == "" || (text == "" && len(atts) == 0) {
		return
	}
	client.mu.Lock()
	agentID := client.agentID
	deviceID := client.deviceID
	client.mu.Unlock()

	if agentID == "" {
		agentID = h.defaultAgentID()
	}
	conn := h.PrimaryBot(agentID)
	if conn == nil {
		log.Warnf("app", "no agent for inbound message (agent=%q)", agentID)
		client.sendRaw(fap.ErrorFrame{
			ConversationID: convID,
			Code:           "no_agent",
			Message:        "no agent is available for this conversation",
		})
		return
	}
	// Stick the resolved agent to the socket for subsequent messages.
	client.mu.Lock()
	if client.agentID == "" {
		client.agentID = agentID
	}
	client.mu.Unlock()

	b := h.ensureBinding(client, agentID, convID)
	text, atts = h.transcribeVoice(conn, text, atts)
	if text == "" && len(atts) == 0 {
		return // voice transcription yielded nothing and there's no other content
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
