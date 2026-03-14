package main

import (
	"fmt"
	"os"
	"time"

	"foci/internal/agent"
	"foci/internal/tempdir"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
)

// wireAgentPlatformCallbacks wires notification callbacks from the agent to all
// platform connections. Uses the Messaging facade for fan-out — zero platform-specific types.
func wireAgentPlatformCallbacks(
	ag *agent.Agent,
	acfg config.AgentConfig,
	cfg *config.Config,
	plat *platform.Messaging,
	connMgr platform.ConnectionManager,
	sessionIndex *session.SessionIndex,
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

	// Task list notify — session-specific connection, falls back to all
	taskListNotify := acfg.TaskListNotify
	if taskListNotify == nil || *taskListNotify {
		ag.TaskListNotifyFunc.Add(func(sk, msg string) {
			if c := connMgr.ForSession(sk); c != nil {
				c.SendNotification(msg)
			} else {
				plat.NotifyAgent(acfg.ID, msg)
			}
		})
	}

	// Compaction notify — session-specific connection, falls back to all
	compactNotify := cfg.Sessions.CompactionNotify
	if acfg.CompactionNotify != nil {
		compactNotify = acfg.CompactionNotify
	}
	if compactNotify == nil || *compactNotify {
		ag.CompactionNotifyFunc.Add(func(sk, msg string) {
			if c := connMgr.ForSession(sk); c != nil {
				c.SendNotification(msg)
			} else {
				plat.NotifyAgent(acfg.ID, msg)
			}
		})
	}

	// Compaction debug — session-specific connection for document
	compactDebug := cfg.Debug.CompactionDebug
	if acfg.CompactionDebug != nil {
		compactDebug = *acfg.CompactionDebug
	}
	if compactDebug {
		ag.CompactionDebugFunc.Add(func(sk, summary string) {
			c := connMgr.ForSession(sk)
			if c == nil {
				c = connMgr.Primary(acfg.ID)
			}
			if c == nil {
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
			if err := c.SendDocument(f.Name()); err != nil {
				log.Warnf("agent", "compaction debug: send document: %v", err)
			}
			_ = os.Remove(f.Name())
		})
	}

	// Session key rotation — update platform caches
	ag.SessionKeyRotatedFunc.Add(func(oldKey, newKey string) {
		chatID := session.ChatIDFromKey(oldKey)
		if chatID == 0 {
			return
		}
		if conn := connMgr.ForSession(oldKey); conn != nil {
			conn.UpdateChatSessionKey(chatID, newKey)
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

func (a *sessionBranchAdapter) CreateBranch(parentKey, branchKey string, opts tools.BranchOptions) error {
	return a.store.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		NoResetHook:        opts.NoResetHook,
		OrientationMessage: opts.OrientationMessage,
	})
}

func (a *sessionBranchAdapter) SessionPath(key string) (string, error) {
	return a.store.SessionPath(key)
}
