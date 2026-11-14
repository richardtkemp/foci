package main

import (
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/relogin"
)

// Advance warning that the Claude Code sign-in is ending (#2864). The decision
// is relogin.ExpiryWarner's; each Claude Code account has its own
// (login_account.go, #3104 Q16), and this file delivers.

// hostLoginExpiryFile is the host account's state file, the one main has
// always used, so an upgraded install keeps the stages it already sent.
const hostLoginExpiryFile = "cc-login-expiry.json"

// loginExpiryTarget is the agent whose chat gets the notice. masterAgent is
// the master agent when it is on the account the notice is about (else "",
// masterOnAccount): its chat is then the right one, as any agent can be the
// first to report the date. Otherwise the reporting agent, which is on that
// account (Dick 2026-11-14, #3104 Q16).
func loginExpiryTarget(masterAgent, agentID string) string {
	if masterAgent != "" {
		return gwAgentRenames.resolve(masterAgent)
	}
	return agentID
}

// deliverLoginExpiryNotice sends the notice to the target agent's primary chat.
// It returns false when nothing reached a chat, so the warner tries again later:
// at the startup probe the connection may not be up yet.
func deliverLoginExpiryNotice(connMgr platform.ConnectionManager, target, notice string) bool {
	conn := connMgr.Primary(target)
	if conn == nil || platform.DropsAgentWideSends(conn) {
		log.NewComponentLogger("agent:" + target).Debugf("sign-in expiry notice undelivered, no chat to address")
		return false
	}
	conn.SendNotification(notice)
	return true
}

// observeLoginExpiry hands one reported date to the warner. A sign-in already
// being asked for (loginPending) needs no second message about it.
func observeLoginExpiry(warner *relogin.ExpiryWarner, connMgr platform.ConnectionManager, target string, loginPending func() bool, expiresAt, now time.Time) {
	if loginPending() {
		return
	}
	warner.Observe(expiresAt, now, func(notice string) bool {
		return deliverLoginExpiryNotice(connMgr, target, notice)
	})
}

// loginExpiryHook builds delegator.HostHooks.OnLoginExpiry. It returns at once:
// the hook runs on the path that reads Claude Code's stdout, and a send can
// block.
// loginPending is the account's gate's Pending.
func loginExpiryHook(warner *relogin.ExpiryWarner, connMgr platform.ConnectionManager, masterAgent, agentID string, loginPending func() bool) func(time.Time) {
	return func(expiresAt time.Time) {
		go observeLoginExpiry(warner, connMgr, loginExpiryTarget(masterAgent, agentID), loginPending, expiresAt, time.Now())
	}
}
