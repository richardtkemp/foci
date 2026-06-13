package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tempdir"
	"foci/internal/tools"
)

// wireAgentPlatformCallbacks wires notification callbacks from the agent to all
// platform connections. Uses the Messaging facade for fan-out — zero platform-specific types.
func wireAgentPlatformCallbacks(
	ag *agent.Agent,
	acfg config.AgentConfig,
	resolved *config.ResolvedAgentConfig,
	plat *platform.Messaging,
	connMgr platform.ConnectionManager,
	sessionIndex *session.SessionIndex,
	tmuxMigrateKey func(string, string),
) {
	// Register ALL platform connections with agent
	for i, conn := range connMgr.AllForAgent(acfg.ID) {
		ag.AddPlatform(fmt.Sprintf("platform-%d", i), conn)
	}

	// Cache bust — notify all connections
	if ag.CacheBustDetect {
		ag.CacheBustAlert.Add(func(sess string, prev, cur int) {
			msg := fmt.Sprintf("⚠️ %s: cache bust, cache_read dropped %d → %d", sess, prev, cur)
			log.Warnf("agent", "%s", msg)
			plat.NotifyAgent(acfg.ID, msg)
		})
	}

	// Mana warnings — notify all
	if ag.ManaWatcher != nil {
		ag.ManaWarnFunc.Add(func(warn string) {
			log.Infof("mana", "%s", warn)
			plat.NotifyAgent(acfg.ID, "⚠️ "+warn)
		})
	}

	// Rate limit — notify all
	ag.RateLimitFunc.Add(func(resetTime time.Time) {
		resetStr := mana.ParseResetTime(resetTime.Format(time.RFC3339Nano))
		if resetStr == "" {
			resetStr = resetTime.Format(time.Kitchen)
		}
		plat.NotifyAgent(acfg.ID, fmt.Sprintf("⚡ Rate limited (resets %s).", resetStr))
	})

	// Max tokens — notify all
	ag.MaxTokensWarnFunc.Add(func(warn string) {
		plat.NotifyAgent(acfg.ID, "⚠️ "+warn)
	})

	// Task list notify — per-platform resolution.
	ag.TaskListNotifyFunc.Add(func(sk, msg string) {
		if c := connMgr.ForSession(sk); c != nil {
			if resolved.PlatformNotify(c.PlatformName()).TaskListNotify {
				c.SendNotification(msg)
			}
		} else {
			for _, conn := range connMgr.AllForAgent(acfg.ID) {
				if resolved.PlatformNotify(conn.PlatformName()).TaskListNotify {
					conn.SendNotification(msg)
				}
			}
		}
	})

	// Reset notify — send "please wait" during delegated session reset.
	// Uses SendNotificationDirect to bypass turn buffering (same as compaction start).
	ag.ResetNotifyFunc.Add(func(sk, msg string) {
		if c := connMgr.ForSessionOrPrimary(sk, acfg.ID); c != nil {
			_ = c.SendText(msg)
		}
	})

	// Compaction notify — per-platform resolution.
	// Start notifications use SendNotificationDirect to bypass turn buffering
	// so ⏳ arrives before the compaction completes (not batched with ✅).
	// The msg ID is stored so the completion notification can edit in-place.
	var compactionMsgIDs sync.Map // sessionKey → msgID string
	ag.CompactionStartFunc.Add(func(sk, msg string) {
		if c := connMgr.ForSession(sk); c != nil {
			if resolved.PlatformNotify(c.PlatformName()).CompactionNotify {
				if msgID := c.SendNotificationDirect(msg); msgID != "" {
					compactionMsgIDs.Store(sk, msgID)
				}
			}
		} else {
			for _, conn := range connMgr.AllForAgent(acfg.ID) {
				if resolved.PlatformNotify(conn.PlatformName()).CompactionNotify {
					if msgID := conn.SendNotificationDirect(msg); msgID != "" {
						compactionMsgIDs.Store(sk, msgID)
					}
				}
			}
		}
	})
	ag.CompactionNotifyFunc.Add(func(sk, msg string) {
		// Try to edit the ⏳ start message in-place rather than sending a new one.
		if rawID, ok := compactionMsgIDs.LoadAndDelete(sk); ok {
			msgID := rawID.(string)
			c := connMgr.ForSession(sk)
			if c == nil {
				if conns := connMgr.AllForAgent(acfg.ID); len(conns) > 0 {
					c = conns[0]
				}
			}
			if c != nil {
				if bs, ok := c.(platform.ButtonSender); ok {
					if err := bs.EditMessageText(msgID, msg); err == nil {
						return
					}
					log.Debugf("agent", "compaction edit failed for session=%s msgID=%s, falling back to new message", sk, msgID)
				}
			}
		}
		// Fallback: send as a new message.
		if c := connMgr.ForSession(sk); c != nil {
			if resolved.PlatformNotify(c.PlatformName()).CompactionNotify {
				c.SendNotification(msg)
			}
		} else {
			for _, conn := range connMgr.AllForAgent(acfg.ID) {
				if resolved.PlatformNotify(conn.PlatformName()).CompactionNotify {
					conn.SendNotification(msg)
				}
			}
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
		if !resolved.PlatformNotify(c.PlatformName()).CompactionDebug {
			return
		}
		f, err := os.CreateTemp(tempdir.Dir(), "compaction-summary-*.md")
		if err != nil {
			log.Warnf("agent", "compaction debug: create temp file: %v", err)
			return
		}
		if _, err := f.WriteString(summary); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			log.Warnf("agent", "compaction debug: write temp file: %v", err)
			return
		}
		_ = f.Close()
		if err := c.SendDocument(f.Name(), ""); err != nil {
			log.Warnf("agent", "compaction debug: send document: %v", err)
		}
		_ = os.Remove(f.Name())
	})

	// Session key rotation — update DB directly and tmux ownership
	ag.SessionKeyRotatedFunc.Add(func(oldKey, newKey string) {
		if tmuxMigrateKey != nil {
			tmuxMigrateKey(oldKey, newKey)
		}
		chatID := session.ChatIDFromKey(oldKey)
		if chatID == 0 || sessionIndex == nil {
			return
		}
		if err := sessionIndex.RotateChatSessionKey(acfg.ID, chatID, oldKey, newKey); err != nil {
			log.Errorf("agent", "rotate chat session key %s → %s: %v", oldKey, newKey, err)
		}
	})

	// Session activity tracking
	if sessionIndex != nil {
		ag.OnActivity.Add(func(sk string) { sessionIndex.TouchActivity(sk) })
	}
}

// sessionBranchAdapter wraps session.Store to implement tools.SessionBrancher.
type sessionBranchAdapter struct {
	store *session.Store
}

func (a *sessionBranchAdapter) CreateBranch(parentKey string, opts tools.BranchOptions) (string, error) {
	return a.store.CreateBranchWithOptions(parentKey, session.BranchOptions{
		NoResetHook:         opts.NoResetHook,
		BranchType:          opts.BranchType,
		OrientationTemplate: opts.OrientationTemplate,
	})
}

func (a *sessionBranchAdapter) SessionPath(key string) (string, error) {
	return a.store.SessionPath(key)
}
