package app

import "foci/internal/fap"

// Persistent voice mode (#3047). Dick, 2026-11-11: when the user taps the
// continuous voice input button left of the composer, "any messages sent by
// the agent after that should be sent as voice and should play automatically."
//
// Each socket reports the conversations its device has in voice mode
// (fap.VoiceModePut, a full replace). While any live socket names a
// conversation, the app sink speaks every agent bubble delivered to it through
// the same per-bubble voiceMode clip a voice turn's reply gets (#1439, #1444,
// #2985), whatever the turn's trigger; appConn.SendToSession does the same for
// a message that bypasses the sink. The client already auto-plays every live
// voiceMode clip and queues it behind the audio and voice input in progress
// (#3022, #3031, #3029), so nothing new is needed to play it.
//
// The sending socket's report also decides whether a voice note or spoken text
// it sends is a voice turn (wsClient.inVoiceMode, #3048): a one-off voice note
// from a device not in voice mode is a plain transcribed text turn.
//
// The report is socket state, not durable, exactly like viewing presence: a
// closed socket stops counting at once (removeClient drops the wsClient), and a
// reconnecting client re-sends its report after the hello.

// handleVoiceModePut records the conversations client has in voice mode.
func (h *Hub) handleVoiceModePut(client *wsClient, f fap.VoiceModePut) {
	set := make(map[string]struct{}, len(f.ConversationIDs))
	for _, id := range f.ConversationIDs {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	client.mu.Lock()
	client.voiceMode = set
	dev := client.deviceID
	client.mu.Unlock()
	appLog.Infof("voice mode: device=%s conversations=%v", dev, f.ConversationIDs)
}

// voiceModeOn reports whether any live socket has convID in voice mode.
func (h *Hub) voiceModeOn(convID string) bool {
	if convID == "" {
		return false
	}
	for _, c := range h.snapshotClients() {
		c.mu.Lock()
		_, on := c.voiceMode[convID]
		c.mu.Unlock()
		if on {
			return true
		}
	}
	return false
}

// inVoiceMode reports whether this socket's device has convID in voice mode.
// Only such a device's voice note or spoken text is a voice turn (#3048).
func (c *wsClient) inVoiceMode(convID string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, on := c.voiceMode[convID]
	return on
}
