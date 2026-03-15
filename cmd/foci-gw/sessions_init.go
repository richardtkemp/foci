package main

import (
	"os"
	"path/filepath"
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
			case session.SessionStatusRotated:
				// Index the archive file
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
				// Create new active entry for the rotated key
				if e.NewKey != "" {
					sessionIndex.Upsert(session.SessionIndexEntry{
						SessionKey:  e.NewKey,
						FilePath:    e.FilePath,
						CreatedAt:   time.Now().UTC(),
						SessionType: e.Type,
						Status:      session.SessionStatusActive,
					})
				}
				// Mark old key as rotated
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusRotated)
			case session.SessionStatusCleared:
				sessionIndex.UpdateStatus(e.Key, session.SessionStatusCleared)
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

	cleanupLegacyStateKeys(stateStore, sessions)

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

// cleanupLegacyStateKeys migrates colon-separated agent keys to slash format
// and removes stale no_compact entries for branch sessions that no longer exist.
func cleanupLegacyStateKeys(stateStore *state.Store, sessions *session.Store) {
	keys := stateStore.AllKeys()
	var toDelete []string

	for _, k := range keys {
		// Remove stale no_compact entries for branch/spawn sessions whose
		// session files no longer exist on disk.
		if strings.HasPrefix(k, "no_compact/") {
			sessionKey := strings.TrimPrefix(k, "no_compact/")
			path, err := sessions.SessionPath(sessionKey)
			if err != nil {
				toDelete = append(toDelete, k)
				continue
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				toDelete = append(toDelete, k)
			}
		}
	}

	if len(toDelete) > 0 {
		if err := stateStore.DeleteKeys(toDelete); err != nil {
			log.Warnf("main", "cleanup legacy state keys: %v", err)
		} else {
			log.Infof("main", "cleaned up %d stale state key(s)", len(toDelete))
		}
	}
}
