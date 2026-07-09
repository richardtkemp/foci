package route

import "foci/internal/platform"

// NotifySessionChat delivers text to the chat owning sessionKey via the
// resolved connection. Uses SessionNotifier where the transport supports it
// (Telegram, Discord — single bot serving multiple chats, chat ID parsed from
// the session key); otherwise falls back to SendNotification (app, where bound
// connections already route to the correct chat). No-op if no connection is
// available for the session or agent.
func NotifySessionChat(cm platform.ConnectionManager, agentID, sessionKey, text string) {
	conn := cm.ForSessionOrPrimary(sessionKey, agentID)
	if conn == nil {
		return
	}
	if sn, ok := conn.(platform.SessionNotifier); ok {
		sn.SendNotificationToSession(sessionKey, text)
	} else {
		conn.SendNotification(text)
	}
}
