package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/tempdir"
	"foci/internal/tools"
)

var (
	agentLog     = log.NewComponentLogger("agent")
	askLog       = log.NewComponentLogger("ask")
	askgwLog     = log.NewComponentLogger("askgw")
	bitwardenLog = log.NewComponentLogger("bitwarden")
	branchLog    = log.NewComponentLogger("branch")
	configLog    = log.NewComponentLogger("config")
	httpLog      = log.

		// wireAgentPlatformCallbacks wires notification callbacks from the agent to all
		// platform connections. Uses the Messaging facade for fan-out — zero platform-specific types.
		NewComponentLogger("http")
	keepaliveLog = log.NewComponentLogger("keepalive")
	mainLog      = log.NewComponentLogger("main")
	modelcapsLog = log.NewComponentLogger("modelcaps")
	modelinfoLog = log.NewComponentLogger("modelinfo")
	nudgeLog     = log.NewComponentLogger("nudge")
	remindLog    = log.NewComponentLogger("remind")
	rotateLog    = log.NewComponentLogger("rotate")
	securityLog  = log.NewComponentLogger("security")
	setupLog     = log.NewComponentLogger(

		// Register ALL platform connections with agent
		"setup")
	startupLog             = log.NewComponentLogger("startup")
	testharness_controlLog = log.NewComponentLogger("testharness_control")
	warningLog             = log.NewComponentLogger(

		// broadcastNotify fans a notice out to every live connection for this
		// agent (route.Broadcast — the delivery set behind PolicyBroadcast).
		"warning")
)

func wireAgentPlatformCallbacks(
	ag *agent.Agent,
	acfg config.AgentConfig,
	resolvedLive *config.LiveValue[*config.ResolvedAgentConfig],
	connMgr platform.ConnectionManager,
	sessionIndex *session.SessionIndex,
) {

	for i, conn := range connMgr.AllForAgent(acfg.ID) {
		ag.AddPlatform(fmt.Sprintf("platform-%d", i), conn)
	}

	broadcastNotify := func(text string) {
		for _, conn := range route.Broadcast(connMgr, acfg.ID) {
			conn.SendNotification(text)
		}
	}

	// Cache bust — session-specific, not a broadcast: the bust concerns ONE
	// session's cache prefix, so the notice goes to that session's chat
	// (SessionNotifier where supported, #911), not to every surface.
	if ag.CacheBustDetect {
		ag.CacheBustAlert.Add(func(sess string, prev, cur int) {
			msg := fmt.Sprintf("⚠️ %s: cache bust, cache_read dropped %d → %d", sess, prev, cur)
			agentLog.Warnf("%s", msg)
			route.NotifySessionChat(connMgr, acfg.ID, sess, msg)
		})
	}

	// Rate limit — broadcast to every surface
	ag.RateLimitFunc.Add(func(resetTime time.Time) {
		broadcastNotify(fmt.Sprintf("⚡ Rate limited (resets %s).", resetTime.Format(time.Kitchen)))
	})

	// Max tokens — broadcast to every surface
	ag.MaxTokensWarnFunc.Add(func(warn string) {
		broadcastNotify("⚠️ " + warn)
	})

	// Task list notify — per-platform resolution.
	ag.TaskListNotifyFunc.Add(func(sk, msg string) {
		if c := connMgr.ForSession(sk); c != nil {
			if resolvedLive.Load().PlatformNotify(c.PlatformName()).TaskListNotify {
				c.SendNotification(msg)
			}
		} else {
			for _, conn := range connMgr.AllForAgent(acfg.ID) {
				if resolvedLive.Load().PlatformNotify(conn.PlatformName()).TaskListNotify {
					conn.SendNotification(msg)
				}
			}
		}
	})

	// Compaction notify — per-platform resolution.
	// Start notifications use SendNotificationDirect to bypass turn buffering
	// so ⏳ arrives before the compaction completes (not batched with ✅).
	// The msg ID is stored so the completion notification can edit in-place.
	var compactionMsgIDs sync.Map // sessionKey → msgID string
	ag.CompactionStartFunc.Add(func(sk, msg string) {
		// Route to the connection owning THIS session, not a blind broadcast to the
		// default chat. ForSessionOrPrimary is platform-aware (returns the owning
		// platform's primary even for a non-default user), and a SessionNotifier
		// delivers to that user's chat rather than the default one (#911).
		c := connMgr.ForSessionOrPrimary(sk, acfg.ID)
		if c == nil || !resolvedLive.Load().PlatformNotify(c.PlatformName()).CompactionNotify {
			return
		}
		var msgID string
		if sn, ok := c.(platform.SessionNotifier); ok {
			msgID = sn.SendNotificationToSession(sk, msg)
		} else {
			msgID = c.SendNotificationDirect(msg)
		}
		if msgID != "" {
			compactionMsgIDs.Store(sk, msgID)
		}
	})
	ag.CompactionNotifyFunc.Add(func(sk, msg, summary string) {
		c := connMgr.ForSessionOrPrimary(sk, acfg.ID)
		if c == nil || !resolvedLive.Load().PlatformNotify(c.PlatformName()).CompactionNotify {
			return
		}
		// Try to edit the ⏳ start message in-place rather than sending a new one.
		// The edit must target the SESSION's chat (#911): editing in the default
		// chat would hit the wrong chat and Telegram would reject the msgID.
		if rawID, ok := compactionMsgIDs.LoadAndDelete(sk); ok {
			msgID := rawID.(string)
			// A summary is available and the connection supports attaching it
			// (currently app-only) — attach it to the existing message so the
			// client can render a tappable "view summary" chit. Falls through
			// to the plain edit below on failure (unknown/consumed msgID, etc).
			if summary != "" {
				if da, ok := c.(platform.DetailAttacher); ok {
					if err := da.AttachDetail(msgID, msg, summary); err == nil {
						agentLog.Debugf("compaction detail attached for session=%s msgID=%s", sk, msgID)
						return
					}
					agentLog.Debugf("compaction detail attach failed for session=%s msgID=%s, falling back to plain edit", sk, msgID)
				}
			}
			if sn, ok := c.(platform.SessionNotifier); ok {
				if err := sn.EditNotificationInSession(sk, msgID, msg); err == nil {
					agentLog.Debugf("compaction edit delivered for session=%s msgID=%s", sk, msgID)
					return
				}
				agentLog.Debugf("compaction edit failed for session=%s msgID=%s, falling back to new message", sk, msgID)
			} else if bs, ok := c.(platform.ButtonSender); ok {
				if err := bs.EditMessageText(msgID, msg); err == nil {
					agentLog.Debugf("compaction edit delivered for session=%s msgID=%s", sk, msgID)
					return
				}
				agentLog.Debugf("compaction edit failed for session=%s msgID=%s, falling back to new message", sk, msgID)
			}
		}
		// Fallback: send as a new message, session-aware where supported.
		if sn, ok := c.(platform.SessionNotifier); ok {
			sn.SendNotificationToSession(sk, msg)
		} else {
			c.SendNotification(msg)
		}
	})

	// Compaction debug — per-platform resolution.
	ag.CompactionDebugFunc.Add(func(sk, summary string) {
		c := connMgr.ForSession(sk)
		if c == nil {
			c = connMgr.Primary(acfg.ID)
		}
		if c == nil {
			return
		}
		if !resolvedLive.Load().PlatformNotify(c.PlatformName()).CompactionDebug {
			return
		}
		f, err := tempdir.Create("compaction-summary-*.md")
		if err != nil {
			agentLog.Warnf("compaction debug: create temp file: %v", err)
			return
		}
		if _, err := f.WriteString(summary); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			agentLog.Warnf("compaction debug: write temp file: %v", err)
			return
		}
		_ = f.Close()
		if err := c.SendDocument(f.Name(), ""); err != nil {
			agentLog.Warnf("compaction debug: send document: %v", err)
		}
		_ = os.Remove(f.Name())
	})

	// Session activity tracking
	if sessionIndex != nil {
		ag.OnActivity.Add(func(sk string) { sessionIndex.TouchActivity(sk) })
	}
}

// sessionBranchAdapter implements tools.SessionBrancher, routing forks through
// Agent.ForkSession (backend fork vs API branch) and session paths through the store.
type sessionBranchAdapter struct {
	store *session.Store
	ag    func() *agent.Agent
}

func (a *sessionBranchAdapter) ForkSession(ctx context.Context, parentKey string, opts tools.BranchOptions) (string, bool, error) {
	return a.ag().ForkSession(ctx, parentKey, session.BranchOptions{
		NoResetHook:         opts.NoResetHook,
		BranchType:          opts.BranchType,
		OrientationTemplate: opts.OrientationTemplate,
	})
}

func (a *sessionBranchAdapter) SessionPath(key string) (string, error) {
	return a.store.SessionPath(key)
}
