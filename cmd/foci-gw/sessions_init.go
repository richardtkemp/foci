package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/startup"
	"foci/internal/timeutil"
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
			log.Warnf("main", "invalid sessions.file_mode %q: %v (using default 0600)", cfg.Sessions.FileMode, err)
		} else {
			sessions.SetFileMode(os.FileMode(mode))
			log.Debugf("main", "session file mode=%04o", mode)
		}
	}
	log.Debugf("main", "session store dir=%s", cfg.Sessions.Dir)

	// One-shot legacy migration: flatten pre-stable-identity version
	// directories before anything reads or indexes session files.
	if n, err := sessions.MigrateLegacyLayout(); err != nil {
		log.Errorf("main", "legacy session layout migration: %v", err)
	} else if n > 0 {
		log.Infof("main", "migrated %d session(s) from legacy version-directory layout", n)
	}

	// State database (SQLite-backed state for sessions, agents, chats, and system)
	stateDBPath := cfg.DataPath("state.db")
	sessionIndex, err := session.NewSessionIndex(stateDBPath)
	if err != nil {
		log.Errorf("main", "create session index: %v (session index disabled)", err)
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
				if e.ArchivePath != "" {
					rel, err := filepath.Rel(cfg.Sessions.Dir, e.ArchivePath)
					if err == nil {
						archiveKey := strings.TrimSuffix(rel, ".jsonl")
						archiveKey = strings.ReplaceAll(archiveKey, string(filepath.Separator), ":")
						sessionIndex.Upsert(session.SessionIndexEntry{
							SessionKey:       archiveKey,
							FilePath:         e.ArchivePath,
							CreatedAt:        timeutil.Now(),
							ParentSessionKey: e.Key,
							SessionType:      e.Type,
							Status:           session.SessionStatusCompacted,
						})
					}
				}
			case session.SessionStatusReset:
				sessionIndex.RecordArchive(e.Key, e.ArchivePath, "reset")
				// Index the archived file; the session key itself is stable
				// and its next Append re-activates it.
				if e.ArchivePath != "" {
					rel, err := filepath.Rel(cfg.Sessions.Dir, e.ArchivePath)
					if err == nil {
						archiveKey := strings.TrimSuffix(rel, ".jsonl")
						archiveKey = strings.ReplaceAll(archiveKey, string(filepath.Separator), ":")
						sessionIndex.Upsert(session.SessionIndexEntry{
							SessionKey:       archiveKey,
							FilePath:         e.ArchivePath,
							CreatedAt:        timeutil.Now(),
							ParentSessionKey: e.Key,
							SessionType:      e.Type,
							Status:           session.SessionStatusCompacted,
						})
					}
				}
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusReset)
			case session.SessionStatusCleared:
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusCleared)
			}
		})

		// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts).
		// Runs after event handler is wired so repairs update the index.
		if n, err := sessions.RepairOrphans(); err != nil {
			log.Warnf("main", "session repair: %v", err)
		} else if n > 0 {
			log.Infof("main", "repaired %d orphaned session(s) with interrupted tool calls", n)
		}

		// Rebuild the session index — skip if last shutdown was clean and the
		// DB already has entries (the lifecycle hooks kept it current).
		cleanShutdown := startup.WasCleanShutdown(sessionIndex)
		indexCount := sessionIndex.IndexCount()
		if cleanShutdown && indexCount > 0 {
			log.Infof("main", "session index: %d sessions (from db, clean shutdown)", indexCount)
		} else {
			reason := "crash/reboot"
			if indexCount == 0 {
				reason = "empty index"
			}
			log.Infof("main", "session index: rebuilding (%s)", reason)
			if n, err := sessionIndex.Rebuild(sessions); err != nil {
				log.Warnf("main", "rebuild session index: %v", err)
			} else {
				log.Infof("main", "session index: %d sessions indexed", n)
			}
		}

		// Background integrity sweep — prune index entries for deleted files.
		go func() {
			if n := sessionIndex.PruneOrphans(); n > 0 {
				log.Infof("main", "session index: pruned %d orphan entries", n)
			}
		}()

		// Start archive sweep goroutine
		archiveAfter, err := time.ParseDuration(cfg.Sessions.ArchiveAfter)
		if err != nil {
			log.Warnf("main", "invalid sessions.archive_after %q: %v (archive sweep disabled)", cfg.Sessions.ArchiveAfter, err)
		} else {
			archiveStop := make(chan struct{})
			archiveTicker := time.NewTicker(6 * time.Hour)
			go func() {
				// Run immediately on startup
				if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
					log.Warnf("main", "archive sweep: %v", err)
				} else if n > 0 {
					log.Infof("main", "archive sweep: archived %d idle session(s)", n)
				}
				for {
					select {
					case <-archiveTicker.C:
						if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
							log.Warnf("main", "archive sweep: %v", err)
						} else if n > 0 {
							log.Infof("main", "archive sweep: archived %d idle session(s)", n)
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
		log.Warnf("main", "migrate state.json: read: %v", err)
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Warnf("main", "migrate state.json: parse: %v", err)
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
			// Check for session metadata patterns: <prefix>/<sessionKey>
			sessionMetaPrefixes := []string{
				"effort", "thinking", "speed", "model", "model_endpoint",
				"model_format", "show_tool_calls", "display_show_thinking",
				"stream_output", "display_width", "no_compact",
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
				log.Debugf("main", "migrate state.json: skipping unknown key %q", key)
			}
		}
		migrated++
	}

	// Rename to .migrated
	migratedPath := jsonPath + ".migrated"
	if err := os.Rename(jsonPath, migratedPath); err != nil {
		log.Warnf("main", "migrate state.json: rename: %v", err)
	} else {
		log.Infof("main", "migrated %d state.json keys to SQLite, renamed to %s", migrated, filepath.Base(migratedPath))
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
	keys, err := idx.SessionKeysWithMetadata("no_compact")
	if err != nil {
		log.Warnf("main", "query stale session metadata: %v", err)
		return
	}
	var deleted int
	for _, sk := range keys {
		path, err := sessions.SessionPath(sk)
		if err != nil {
			_ = idx.DeleteSessionMetadata(sk, "no_compact")
			deleted++
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_ = idx.DeleteSessionMetadata(sk, "no_compact")
			deleted++
		}
	}
	if deleted > 0 {
		log.Infof("main", "cleaned up %d stale no_compact entries", deleted)
	}
}
