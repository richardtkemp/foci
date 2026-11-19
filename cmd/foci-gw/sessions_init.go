package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/startup"
)

type sessionInfra struct {
	sessions     *session.Store
	sessionIndex *session.SessionIndex
	cleanup      func()
}

// initSessions creates the session store, session index (SQLite), and state store.
// Repairs orphaned tool calls, rebuilds the session index, and starts the
// archive sweep goroutine.
func initSessions(cfg *config.Config) sessionInfra {
	var cleanups []func()

	// Session store
	sessions := session.NewStore(cfg.Sessions.Dir)
	if cfg.Sessions.FileMode != "" {
		mode, err := strconv.ParseUint(cfg.Sessions.FileMode, 8, 32)
		if err != nil {
			mainLog.Warnf("invalid sessions.file_mode %q: %v (using default 0600)", cfg.Sessions.FileMode, err)
		} else {
			sessions.SetFileMode(os.FileMode(mode))
			mainLog.Debugf("session file mode=%04o", mode)
		}
	}
	mainLog.Debugf("session store dir=%s", cfg.Sessions.Dir)

	// One-shot legacy migration: flatten pre-stable-identity version
	// directories before anything reads or indexes session files.
	if n, err := sessions.MigrateLegacyLayout(); err != nil {
		mainLog.Errorf("legacy session layout migration: %v", err)
	} else if n > 0 {
		mainLog.Infof("migrated %d session(s) from legacy version-directory layout", n)
	}

	// State database (SQLite-backed state for sessions, agents, chats, and system)
	stateDBPath := cfg.DataPath("state.db")
	sessionIndex, err := session.NewSessionIndex(stateDBPath)
	if err != nil {
		mainLog.Errorf("create session index: %v (session index disabled)", err)
	} else {
		cleanups = append(cleanups, func() { _ = sessionIndex.Close() })

		// Wire lifecycle events BEFORE repair so repair events update the index.
		sessions.OnSessionEvent(func(e session.SessionEvent) {
			switch e.Status {
			case session.SessionStatusActive:
				sessionIndex.Upsert(session.SessionIndexEntry{
					SessionKey:       e.Key,
					FilePath:         e.FilePath,
					CreatedAt:        e.CreatedAt,
					ParentSessionKey: e.ParentKey,
					SessionType:      e.Type,
					Status:           session.SessionStatusActive,
				})
			case session.SessionStatusCompacted:
				sessionIndex.RecordArchive(e.Key, e.ArchivePath, "compaction")
				indexArchive(sessionIndex, cfg.Sessions.Dir, e)
			case session.SessionStatusReset:
				sessionIndex.RecordArchive(e.Key, e.ArchivePath, "reset")
				// Index the archived file; the session key itself is stable
				// and its next Append re-activates it.
				indexArchive(sessionIndex, cfg.Sessions.Dir, e)
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusReset)
			case session.SessionStatusCleared:
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusCleared)
			}
		})

		// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts).
		// Runs after event handler is wired so repairs update the index.
		if n, err := sessions.RepairOrphans(); err != nil {
			mainLog.Warnf("session repair: %v", err)
		} else if n > 0 {
			mainLog.Infof("repaired %d orphaned session(s) with interrupted tool calls", n)
		}

		// Rebuild the session index — skip if last shutdown was clean and the
		// DB already has entries (the lifecycle hooks kept it current).
		cleanShutdown := startup.WasCleanShutdown(sessionIndex)
		indexCount := sessionIndex.IndexCount()
		if cleanShutdown && indexCount > 0 {
			mainLog.Infof("session index: %d sessions (from db, clean shutdown)", indexCount)
		} else {
			reason := "crash/reboot"
			if indexCount == 0 {
				reason = "empty index"
			}
			mainLog.Infof("session index: rebuilding (%s)", reason)
			if n, err := sessionIndex.Rebuild(sessions); err != nil {
				mainLog.Warnf("rebuild session index: %v", err)
			} else {
				mainLog.Infof("session index: %d sessions indexed", n)
			}
		}

		// Background integrity sweep — prune index entries for deleted files.
		// Logged even when nothing was pruned: the line marks the sweep done,
		// which the L2 tests of rows surviving a restart wait for (#3104).
		go func() {
			mainLog.Infof("session index: pruned %d orphan entries", sessionIndex.PruneOrphans())
		}()

		// Start archive sweep goroutine
		archiveAfter, err := time.ParseDuration(cfg.Sessions.ArchiveAfter)
		if err != nil {
			mainLog.Warnf("invalid sessions.archive_after %q: %v (archive sweep disabled)", cfg.Sessions.ArchiveAfter, err)
		} else {
			archiveStop := make(chan struct{})
			archiveTicker := time.NewTicker(6 * time.Hour)
			go func() {
				// Run immediately on startup
				if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter, openChatArchiveHold(sessionIndex)); err != nil {
					mainLog.Warnf("archive sweep: %v", err)
				} else if n > 0 {
					mainLog.Infof("archive sweep: archived %d idle session(s)", n)
				}
				for {
					select {
					case <-archiveTicker.C:
						if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter, openChatArchiveHold(sessionIndex)); err != nil {
							mainLog.Warnf("archive sweep: %v", err)
						} else if n > 0 {
							mainLog.Infof("archive sweep: archived %d idle session(s)", n)
						}
					case <-archiveStop:
						return
					}
				}
			}()
			cleanups = append(cleanups, func() {
				archiveTicker.Stop()
				close(archiveStop)
			})
		}
	}

	// Migrate state.json → SQLite if it exists
	migrateStateJSON(cfg.DataPath("state.json"), sessionIndex)

	// Clean up stale session metadata
	cleanupStaleSessionMetadata(sessionIndex, sessions)

	return sessionInfra{
		sessions:     sessions,
		sessionIndex: sessionIndex,
		cleanup: func() {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		},
	}
}

// indexArchive writes the row of a compaction/reset archive file. Its key and
// created_at come from session.ArchiveIndexIdentity, the derivation an index
// rebuild uses for the same file, so a row written here and one rebuilt
// from disk agree (#3184). The key once used ':' separators and the clock
// read after the archive was named; a rebuild replaced both.
func indexArchive(idx *session.SessionIndex, storeDir string, e session.SessionEvent) {
	if e.ArchivePath == "" {
		return
	}
	key, createdAt, err := session.ArchiveIndexIdentity(storeDir, e.ArchivePath)
	if err != nil {
		mainLog.Warnf("index archive %s: %v", e.ArchivePath, err)
		return
	}
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:       key,
		FilePath:         e.ArchivePath,
		CreatedAt:        createdAt,
		ParentSessionKey: e.Key,
		SessionType:      e.Type,
		Status:           session.SessionStatusCompacted,
	})
}

// migrateStateJSON performs a one-time migration of state.json key-value data
// into the SQLite session index tables. After migration, renames state.json to
// state.json.migrated. Does NOT import internal/state — reads raw JSON directly.
func migrateStateJSON(jsonPath string, idx *session.SessionIndex) {
	if idx == nil {
		return
	}
	data, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		mainLog.Warnf("migrate state.json: read: %v", err)
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		mainLog.Warnf("migrate state.json: parse: %v", err)
		return
	}

	var migrated int
	for key, val := range raw {
		var strVal string
		// Try to unmarshal as string first; if that fails, use the raw JSON.
		if err := json.Unmarshal(val, &strVal); err != nil {
			strVal = string(val)
		}

		switch {
		case key == "system:last_clean_shutdown":
			_ = idx.SetSystemState("last_clean_shutdown", strVal)

		case strings.HasPrefix(key, "agent/") && strings.HasSuffix(key, "/first_run_completed"):
			// agent/<id>/first_run_completed
			parts := strings.SplitN(key, "/", 3)
			if len(parts) == 3 {
				_ = idx.SetAgentMetadata(parts[1], "first_run_completed", "true")
			}

		case strings.HasPrefix(key, "agent/") && strings.Contains(key, "/chat/") && strings.HasSuffix(key, "/username"):
			// agent/<id>/chat/<cid>/username
			parts := strings.SplitN(key, "/", 5) // agent, <id>, chat, <cid>, username
			if len(parts) == 5 {
				cid, err := strconv.ParseInt(parts[3], 10, 64)
				if err == nil {
					_ = idx.SetChatMetadata(parts[1], "", cid, "username", strVal)
				}
			}

		case strings.HasPrefix(key, "consolidation_last:"):
			// consolidation_last:<id>
			agentID := strings.TrimPrefix(key, "consolidation_last:")
			_ = idx.SetAgentMetadata(agentID, "consolidation_last", strVal)

		case strings.HasPrefix(key, "mana:"):
			// Legacy mana-watcher state — the mana feature was removed; skip.

		case strings.HasPrefix(key, "tmux:") && strings.HasSuffix(key, ":watches"):
			// tmux:<id>:watches
			agentID := strings.TrimPrefix(key, "tmux:")
			agentID = strings.TrimSuffix(agentID, ":watches")
			_ = idx.SetAgentMetadata(agentID, "tmux_watches", string(val))

		case strings.HasPrefix(key, "tmux:"):
			// tmux:<id> (owned map)
			agentID := strings.TrimPrefix(key, "tmux:")
			_ = idx.SetAgentMetadata(agentID, "tmux_owned", string(val))

		case strings.HasPrefix(key, "facet:"):
			_ = idx.SetAgentMetadata("_system", key, strVal)

		case strings.HasPrefix(key, "discord_facet:"):
			_ = idx.SetAgentMetadata("_system", key, strVal)

		default:
			// Check for session metadata patterns: <prefix>/<sessionKey>.
			// Prefix-match order is load-bearing (model before model_endpoint),
			// so keep this order exactly as-is.
			sessionMetaPrefixes := []string{
				session.MetaKeyEffort, session.MetaKeyThinking, session.MetaKeySpeed,
				session.MetaKeyModel, session.MetaKeyModelEndpoint,
				session.MetaKeyModelFormat, session.MetaKeyShowToolCalls,
				session.MetaKeyDisplayShowThinking,
				session.MetaKeyStreamOutput, session.MetaKeyDisplayWidth,
				session.MetaKeyNoCompact,
			}
			handled := false
			for _, prefix := range sessionMetaPrefixes {
				if strings.HasPrefix(key, prefix+"/") {
					sk := strings.TrimPrefix(key, prefix+"/")
					_ = idx.SetSessionMetadata(sk, prefix, strVal)
					handled = true
					break
				}
			}

			// Check for <stateKey>:chatid / <stateKey>:channelid patterns
			if !handled {
				if strings.HasSuffix(key, ":chatid") {
					// Can't determine agent ID from old format — skip
					handled = true
				} else if strings.HasSuffix(key, ":channelid") {
					handled = true
				}
			}

			if !handled {
				mainLog.Debugf("migrate state.json: skipping unknown key %q", key)
			}
		}
		migrated++
	}

	// Rename to .migrated
	migratedPath := jsonPath + ".migrated"
	if err := os.Rename(jsonPath, migratedPath); err != nil {
		mainLog.Warnf("migrate state.json: rename: %v", err)
	} else {
		mainLog.Infof("migrated %d state.json keys to SQLite, renamed to %s", migrated, filepath.Base(migratedPath))
	}

	// Clean up WAL/SHM files from the old state.json (SQLite-style, but state.json was plain JSON)
	// Actually state.json was plain JSON, no WAL files. But clean up if they exist from some transient state.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(jsonPath + suffix)
	}
}

// cleanupStaleSessionMetadata removes no_compact entries for sessions whose
// files no longer exist on disk.
func cleanupStaleSessionMetadata(idx *session.SessionIndex, sessions *session.Store) {
	if idx == nil {
		return
	}
	keys, err := idx.SessionKeysWithMetadata(session.MetaKeyNoCompact)
	if err != nil {
		mainLog.Warnf("query stale session metadata: %v", err)
		return
	}
	var deleted int
	for _, sk := range keys {
		path, err := sessions.SessionPath(sk)
		if err != nil {
			_ = idx.DeleteSessionMetadata(sk, session.MetaKeyNoCompact)
			deleted++
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_ = idx.DeleteSessionMetadata(sk, session.MetaKeyNoCompact)
			deleted++
		}
	}
	if deleted > 0 {
		mainLog.Infof("cleaned up %d stale no_compact entries", deleted)
	}
}

// openChatArchiveHold is the archive sweep's hold (#3134, Dick 2026-11-16:
// "preserve transcripts for any open chat"): a session bound to a chat in the
// app's shared open set stays uncompressed, with no time cap. A chat the
// auto-close sweep closes follows the normal rules again. Built per sweep, so
// each sweep reads the open set afresh, from the store: the startup sweep runs
// before the app hub rebuilds its bindings. An unreadable open set holds
// every session this sweep.
func openChatArchiveHold(idx *session.SessionIndex) func(session.SessionIndexEntry) bool {
	open, err := idx.OpenChatSessionKeys()
	if err != nil {
		mainLog.Warnf("archive sweep: open chats: %v; archiving nothing this sweep", err)
		return func(session.SessionIndexEntry) bool { return true }
	}
	return func(e session.SessionIndexEntry) bool { return open[e.SessionKey] }
}
