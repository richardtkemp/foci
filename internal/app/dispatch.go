package app

import (
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/app/fap"
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

	switch f := in.Frame.(type) {
	case fap.ClientHello:
		client.mu.Lock()
		client.deviceID = f.Client.DeviceID
		client.mu.Unlock()
		client.sendRaw(fap.HelloServer{
			Version: fap.ProtocolVersion,
			Caps:    fap.Caps{Versions: []int{fap.ProtocolVersion}},
			Agents:  h.agentRoster(),
		})

	case fap.ConversationOpen:
		if h.PrimaryBot(f.AgentID) != nil {
			client.mu.Lock()
			client.agentID = f.AgentID
			client.mu.Unlock()
		}

	case fap.ConversationList:
		client.sendRaw(fap.HelloServer{
			Version: fap.ProtocolVersion,
			Caps:    fap.Caps{Versions: []int{fap.ProtocolVersion}},
			Agents:  h.agentRoster(),
		})

	case fap.ClientMessage:
		h.routeUserText(client, f.ConversationID, f.Text)

	case fap.InteractiveResponse:
		h.handleInteractiveResponse(client, f)

	case fap.Ping:
		client.sendRaw(fap.Pong{})

	case fap.ClientTyping, fap.Read, fap.Command:
		// typing: ignored (no upstream surface); read: reliability slice;
		// command: slice 2 (command-registry routing).

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
		seqBefore = atomic.LoadInt64(&b.seq)
	}

	edit, _, ok := platform.HandleInteractiveCallback(f.Data)
	if !ok {
		// Unknown / expired / already-resolved prompt: no callback fired, so no
		// re-registration could have happened — safe to drop any stale entry.
		h.deletePrompt(f.PromptID)
		return
	}
	if b != nil && atomic.LoadInt64(&b.seq) != seqBefore {
		// A follow-up question re-rendered + re-registered this promptID; keep both.
		return
	}
	h.deletePrompt(f.PromptID)
	if edit != "" && b != nil {
		b.send(fap.InteractiveEdit{ConversationID: f.ConversationID, PromptID: f.PromptID, Text: edit})
	}
}

// routeUserText resolves the agent + conversation binding and enqueues a user
// turn. Empty text is ignored.
func (h *Hub) routeUserText(client *wsClient, convID, text string) {
	if text == "" || convID == "" {
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
	conn.agentRef.Enqueue(agent.Envelope{
		SessionKey: b.sessionKey,
		Text:       text,
		UserID:     deviceID,
		ChatID:     b.chatID,
		ReceivedAt: time.Now(),
		Original:   convID,
		Driver:     conn,
	})
}
