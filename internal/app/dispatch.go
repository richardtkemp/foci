package app

import (
	"time"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/log"
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
		// Slice 1 has no native buttons; a typed answer to a text prompt
		// arrives here only if a client sends one — treat the data as message
		// text so it still reaches the agent.
		h.routeUserText(client, f.ConversationID, f.Data)

	case fap.Ping:
		client.sendRaw(fap.Pong{})

	case fap.ClientTyping, fap.Read, fap.Command:
		// typing: ignored (no upstream surface); read: reliability slice;
		// command: slice 2 (command-registry routing).

	default:
		// nil Frame (unknown t) — forward-compat, ignore.
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
