package route

import (
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

var notifyLog = log.NewComponentLogger("route")

// NotifySessionChat delivers text to the chat owning sessionKey via the
// resolved connection. Uses SessionNotifier where the transport supports it
// (Telegram, Discord — single bot serving multiple chats, chat ID parsed from
// the session key); otherwise falls back to SendNotification (app, where bound
// connections already route to the correct chat).
//
// With no connection for the session or agent the notice is lost, and is logged
// at WARN so it does not look delivered (#2769). A cross-agent spawn's session
// has no connection by design (#2662), so that case logs at DEBUG only.
func NotifySessionChat(cm platform.ConnectionManager, agentID, sessionKey, text string) {
	conn := cm.ForSessionOrPrimary(sessionKey, agentID)
	if conn == nil {
		if session.IsCrossAgentSpawnKey(sessionKey) {
			notifyLog.Debugf("no chat for cross-agent spawn session %s, notice not delivered: %s", sessionKey, text)
			return
		}
		notifyLog.Warnf("no connection for agent %s session %s, notice not delivered: %s", agentID, sessionKey, text)
		return
	}
	if sn, ok := conn.(platform.SessionNotifier); ok {
		sn.SendNotificationToSession(sessionKey, text)
	} else {
		conn.SendNotification(text)
	}
}
