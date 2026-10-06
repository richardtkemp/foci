package app

import (
	"slices"

	"foci/internal/fap"
)

// Viewing presence (#2202). Each socket reports the conversation its device is
// showing the user right now (fap.ViewingPut: app foregrounded or window
// focused, and that chat focused), or none. The hub holds it per socket and
// uses it two ways:
//
//   - every socket gets a fap.ViewingSync listing the conversations its user's
//     OTHER devices are viewing, so it posts no local alert for them;
//   - pushNotify sends no offline wake push for a conversation any device is
//     viewing.
//
// Both exist because the user reading a chat on one device used to get an alert
// for it on another, which that reading device's read receipt then cancelled
// 0.3-1.6s later (the phone buzzed for every message of a turn Dick was reading
// on the Mac).
//
// The report is socket state, not durable: a closed socket stops counting at
// once (removeClient), and a reconnecting client re-sends its report after the
// hello.

// handleViewingPut records client's viewed conversation and, when it changed,
// sends every socket its new ViewingSync.
func (h *Hub) handleViewingPut(client *wsClient, f fap.ViewingPut) {
	client.mu.Lock()
	changed := client.viewing != f.ConversationID
	client.viewing = f.ConversationID
	dev := client.deviceID
	client.mu.Unlock()
	appLog.Debugf("viewing: device=%s conv=%q", dev, f.ConversationID)
	if changed {
		h.broadcastViewing()
	}
}

// viewingClient is one socket's device and viewed conversation.
type viewingClient struct {
	c       *wsClient
	device  string
	viewing string
}

// snapshotViewing reads every socket's device and viewed conversation, each
// under its own lock (never nested with h.mu; see connectedDeviceIDs).
func (h *Hub) snapshotViewing() []viewingClient {
	clients := h.snapshotClients()
	out := make([]viewingClient, 0, len(clients))
	for _, c := range clients {
		c.mu.Lock()
		out = append(out, viewingClient{c: c, device: c.deviceID, viewing: c.viewing})
		c.mu.Unlock()
	}
	return out
}

// viewedByOthers lists, sorted and deduplicated, the conversations viewed by
// sockets other than self that belong to a different device. A second socket of
// the same device is not "another device": the device knows what it shows.
func viewedByOthers(all []viewingClient, self *wsClient, selfDevice string) []string {
	ids := []string{}
	for _, v := range all {
		if v.c == self || v.viewing == "" || (selfDevice != "" && v.device == selfDevice) {
			continue
		}
		ids = append(ids, v.viewing)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// broadcastViewing sends each socket the conversations its other devices view.
func (h *Hub) broadcastViewing() {
	all := h.snapshotViewing()
	for _, v := range all {
		v.c.sendRaw(fap.ViewingSync{ConversationIDs: viewedByOthers(all, v.c, v.device)})
	}
}

// pushViewing sends a just-hello'd socket the conversations its other devices
// view. Skipped when there are none: a fresh client already assumes none.
func (h *Hub) pushViewing(client *wsClient) {
	ids := viewedByOthers(h.snapshotViewing(), client, client.device())
	if len(ids) > 0 {
		client.sendRaw(fap.ViewingSync{ConversationIDs: ids})
	}
}

// isViewed reports whether any live socket is viewing convID.
func (h *Hub) isViewed(convID string) bool {
	if convID == "" {
		return false
	}
	for _, v := range h.snapshotViewing() {
		if v.viewing == convID {
			return true
		}
	}
	return false
}
