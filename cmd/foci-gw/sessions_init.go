package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/state"
)

type sessionInfra struct {
	sessions     *session.Store
	sessionIndex *session.SessionIndex
	stateStore   *state.Store
	cleanup      func()
}

// initSessions creates the session store, session index (SQLite), and state store.
// Repairs orphaned tool calls, injects restart markers, rebuilds the session index,
// and starts the archive sweep goroutine.
func initSessions(cfg *config.Config) sessionInfra {
	var cleanups []func()

	// Session store
	sessions := session.NewStore(cfg.Sessions.Dir)
	log.Debugf("main", "session store dir=%s", cfg.Sessions.Dir)

	// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts)
	if n, err := sessions.RepairOrphans(); err != nil {
		log.Warnf("main", "session repair: %v", err)
	} else if n > 0 {
		log.Infof("main", "repaired %d orphaned session(s) with interrupted tool calls", n)
	}

	// Inject restart markers into recently active sessions
	if n, err := sessions.InjectRestartMarkers(session.RestartMarkerMaxAge); err != nil {
		log.Warnf("main", "restart markers: %v", err)
	} else if n > 0 {
		log.Infof("main", "injected restart markers into %d active session(s)", n)
	}

	// State database (SQLite-backed state for sessions, agents, chats, and system)
	stateDBPath := cfg.DataPath("state.db")
	sessionIndex, err := session.NewSessionIndex(stateDBPath)
	if err != nil {
		log.Errorf("main", "create session index: %v (session index disabled)", err)
	} else {
		cleanups = append(cleanups, func() { _ = sessionIndex.Close() })

		// Rebuild index from disk on startup
		if n, err := sessionIndex.Rebuild(sessions); err != nil {
			log.Warnf("main", "rebuild session index: %v", err)
		} else {
			log.Infof("main", "session index: %d sessions indexed", n)
		}

		// Wire lifecycle events from session store to index
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
				if e.ArchivePath != "" {
					rel, err := filepath.Rel(cfg.Sessions.Dir, e.ArchivePath)
					if err == nil {
						archiveKey := strings.TrimSuffix(rel, ".jsonl")
						archiveKey = strings.ReplaceAll(archiveKey, string(filepath.Separator), ":")
						sessionIndex.Upsert(session.SessionIndexEntry{
							SessionKey:       archiveKey,
							FilePath:         e.ArchivePath,
							CreatedAt:        time.Now().UTC(),
							ParentSessionKey: e.Key,
							SessionType:      e.Type,
							Status:           session.SessionStatusCompacted,
						})
					}
				}
			case session.SessionStatusCleared:
				sessionIndex.SetStatus(e.Key, session.SessionStatusCleared)
			}
		})

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

	// State persistence (JSON file in data dir)
	statePath := cfg.DataPath("state.json")
	stateStore := state.New(statePath)
	if err := stateStore.Load(); err != nil {
		log.Errorf("main", "load state: %v", err)
	}

	// ============================================================================
	// TODO REMOVE ME: One-time migration from state.json to session_index.db
	// Migrates agent/chat metadata from JSON state file to database tables.
	// This can be removed after all deployments have migrated (post 2026-03-04).
	// ============================================================================
	if sessionIndex != nil {
		migrateStateToDatabase(stateStore, sessionIndex)
	}
	// ============================================================================
	// END TODO REMOVE ME
	// ============================================================================

	return sessionInfra{
		sessions:     sessions,
		sessionIndex: sessionIndex,
		stateStore:   stateStore,
		cleanup: func() {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		},
	}
}

// ============================================================================
// TODO REMOVE ME: One-time migration from state.json to state.db
// This function can be deleted after all deployments have migrated (post 2026-03-04).
// ============================================================================
func migrateStateToDatabase(stateStore *state.Store, sessionIndex *session.SessionIndex) {
	// Check if migration already done
	marker, _ := sessionIndex.GetSystemState("_migration_state_to_db_done")
	if marker == "true" {
		return
	}

	log.Infof("main", "migrating state.json to state.db...")

	allKeys := stateStore.AllKeys()
	migrated := 0

	for _, oldKey := range allKeys {
		var err error
		var value interface{}
		if !stateStore.Get(oldKey, &value) {
			continue
		}
		valueStr := fmt.Sprintf("%v", value)

		// Parse and route to appropriate table
		parts := strings.Split(oldKey, ":")

		// Pattern: agent:AGENTID:chat:CHATID:KEY → chat_metadata
		if len(parts) == 5 && parts[0] == "agent" && parts[2] == "chat" {
			agentID := parts[1]
			chatID, parseErr := strconv.ParseInt(parts[3], 10, 64)
			if parseErr != nil {
				log.Warnf("main", "skip invalid chat key %q: %v", oldKey, parseErr)
				continue
			}
			key := parts[4]
			err = sessionIndex.SetChatMetadata(agentID, chatID, key, valueStr)
		} else if len(parts) == 3 && parts[0] == "agent" {
			// Pattern: agent:AGENTID:KEY → agent_metadata
			agentID := parts[1]
			key := parts[2]
			err = sessionIndex.SetAgentMetadata(agentID, key, valueStr)
		} else if len(parts) == 3 && parts[0] == "bot" {
			// Pattern: bot:AGENTID:KEY → agent_metadata (bot: is same as agent:)
			agentID := parts[1]
			key := parts[2]
			err = sessionIndex.SetAgentMetadata(agentID, "bot_"+key, valueStr)
		} else if len(parts) == 2 && parts[0] == "consolidation_last" {
			// Pattern: consolidation_last:AGENTID → agent_metadata
			agentID := parts[1]
			err = sessionIndex.SetAgentMetadata(agentID, "consolidation_last", valueStr)
		} else if strings.HasPrefix(oldKey, "no_compact:") {
			// Pattern: no_compact:SESSION_KEY → session_metadata
			sessionKey := strings.TrimPrefix(oldKey, "no_compact:")
			err = sessionIndex.SetSessionMetadata(sessionKey, "no_compact", valueStr)
		} else if strings.HasPrefix(oldKey, "effort:agent:") {
			// Pattern: effort:agent:AGENTID:chat:CHATID → chat_metadata
			suffix := strings.TrimPrefix(oldKey, "effort:agent:")
			suffixParts := strings.Split(suffix, ":")
			if len(suffixParts) == 3 && suffixParts[1] == "chat" {
				agentID := suffixParts[0]
				chatID, parseErr := strconv.ParseInt(suffixParts[2], 10, 64)
				if parseErr == nil {
					err = sessionIndex.SetChatMetadata(agentID, chatID, "effort", valueStr)
				}
			}
		} else if strings.HasPrefix(oldKey, "model:agent:") {
			// Pattern: model:agent:AGENTID:chat:CHATID → chat_metadata
			suffix := strings.TrimPrefix(oldKey, "model:agent:")
			suffixParts := strings.Split(suffix, ":")
			if len(suffixParts) == 3 && suffixParts[1] == "chat" {
				agentID := suffixParts[0]
				chatID, parseErr := strconv.ParseInt(suffixParts[2], 10, 64)
				if parseErr == nil {
					err = sessionIndex.SetChatMetadata(agentID, chatID, "model", valueStr)
				}
			}
		} else if strings.HasPrefix(oldKey, "thinking:agent:") {
			// Pattern: thinking:agent:AGENTID:chat:CHATID → chat_metadata
			suffix := strings.TrimPrefix(oldKey, "thinking:agent:")
			suffixParts := strings.Split(suffix, ":")
			if len(suffixParts) == 3 && suffixParts[1] == "chat" {
				agentID := suffixParts[0]
				chatID, parseErr := strconv.ParseInt(suffixParts[2], 10, 64)
				if parseErr == nil {
					err = sessionIndex.SetChatMetadata(agentID, chatID, "thinking", valueStr)
				}
			}
		} else if strings.HasPrefix(oldKey, "voice:agent:") {
			// Pattern: voice:agent:AGENTID:chat:CHATID → chat_metadata
			// Pattern: voice:agent:AGENTID:KEY → agent_metadata
			suffix := strings.TrimPrefix(oldKey, "voice:agent:")
			suffixParts := strings.Split(suffix, ":")
			if len(suffixParts) == 3 && suffixParts[1] == "chat" {
				agentID := suffixParts[0]
				chatID, parseErr := strconv.ParseInt(suffixParts[2], 10, 64)
				if parseErr == nil {
					err = sessionIndex.SetChatMetadata(agentID, chatID, "voice", valueStr)
				}
			} else if len(suffixParts) == 2 {
				agentID := suffixParts[0]
				key := suffixParts[1]
				err = sessionIndex.SetAgentMetadata(agentID, "voice_"+key, valueStr)
			}
		} else if strings.HasPrefix(oldKey, "tmux:") {
			// Pattern: tmux:AGENTID → agent_metadata, tmux:AGENTID:watches → agent_metadata
			suffix := strings.TrimPrefix(oldKey, "tmux:")
			suffixParts := strings.Split(suffix, ":")
			if len(suffixParts) == 1 {
				agentID := suffixParts[0]
				err = sessionIndex.SetAgentMetadata(agentID, "tmux_state", valueStr)
			} else if len(suffixParts) == 2 {
				agentID := suffixParts[0]
				key := suffixParts[1]
				err = sessionIndex.SetAgentMetadata(agentID, "tmux_"+key, valueStr)
			}
		} else {
			// Everything else → system_state
			err = sessionIndex.SetSystemState(oldKey, valueStr)
		}

		if err == nil {
			migrated++
		} else {
			log.Warnf("main", "migrate state key %q: %v", oldKey, err)
		}
	}

	// Mark migration as done
	_ = sessionIndex.SetSystemState("_migration_state_to_db_done", "true")

	if migrated > 0 {
		log.Infof("main", "migrated %d state keys to database", migrated)
	}
}
// ============================================================================
// END TODO REMOVE ME
// ============================================================================
