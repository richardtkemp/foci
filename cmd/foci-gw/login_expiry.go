package main

import (
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/relogin"
)

// Advance warning that the Claude Code sign-in is ending (#2864). The decision
// is relogin.ExpiryWarner's; this file builds its one instance and delivers.

var (
	loginExpiryWarnerOnce sync.Once
	loginExpiryWarnerInst *relogin.ExpiryWarner
)

// loginExpiryWarner returns the process's warner: all Claude Code agents share
// one credential, so they share one record of what was sent.
func loginExpiryWarner(cfg *config.Config) *relogin.ExpiryWarner {
	loginExpiryWarnerOnce.Do(func() {
		loginExpiryWarnerInst = relogin.NewExpiryWarner(cfg.DataPath("cc-login-expiry.json"))
	})
	return loginExpiryWarnerInst
}

// loginExpiryTarget is the agent whose chat gets the notice. The credential
// belongs to the whole install, so the master agent's chat is the right one when
// there is a master. Any agent can be the first to report the date, and its
// chat may belong to someone who cannot sign in.
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
func loginExpiryHook(warner *relogin.ExpiryWarner, connMgr platform.ConnectionManager, masterAgent, agentID string) func(time.Time) {
	return func(expiresAt time.Time) {
		go observeLoginExpiry(warner, connMgr, loginExpiryTarget(masterAgent, agentID), relogin.G.Pending, expiresAt, time.Now())
	}
}
