package main

import (
	"os"
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/sqlite"
)

// initLogging sets up event logging, log rotation, API DB, and conversation DB.
// Returns a cleanup function that should be deferred.
func initLogging(cfg *config.Config) func() {
	if err := log.Init(log.Config{
		Level:       cfg.Logging.Level,
		EventFile:   cfg.Logging.EventFile,
		APIFile:     cfg.Logging.APIFile,
		PayloadFile: cfg.Logging.PayloadFile,
	}); err != nil {
		log.Fatalf("main", "init logging: %v", err)
	}

	var cleanups []func()
	cleanups = append(cleanups, log.Close)

	// Log rotation
	if cfg.Logging.LogRotation {
		rotPeriod, _ := time.ParseDuration(cfg.Logging.RotationPeriod)
		retPeriod, _ := time.ParseDuration(cfg.Logging.RetentionPeriod)
		maxLineSize, _ := config.ParseByteSize(cfg.Logging.RotationMaxLineSize)
		archiveDir := cfg.Logging.ArchiveDir
		if archiveDir == "" {
			archiveDir = filepath.Join(filepath.Dir(cfg.Logging.EventFile), "archive")
		}
		var files []string
		for _, p := range []string{cfg.Logging.EventFile, cfg.Logging.APIFile, cfg.Logging.PayloadFile} {
			if p != "" {
				files = append(files, p)
			}
		}
		stopRotation := log.StartRotation(log.RotationConfig{
			Period:      rotPeriod,
			Retention:   retPeriod,
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
		})
		cleanups = append(cleanups, stopRotation)
	}

	// API call log (SQLite)
	if cfg.Logging.APIDB != "" {
		if err := log.InitAPIDB(cfg.Logging.APIDB); err != nil {
			log.Fatalf("main", "init API db: %v", err)
		}
		cleanups = append(cleanups, log.CloseAPIDB)
	}

	// Conversation log (per-agent SQLite databases)
	if cfg.Logging.ConversationFile != "" {
		migrateConversationDB(cfg)

		var agentIDs []string
		for _, acfg := range cfg.Agents {
			agentIDs = append(agentIDs, acfg.ID)
		}
		pathFn := func(agentID string) string {
			return sqlite.AgentPath(cfg.Logging.ConversationFile, agentID)
		}
		if err := log.InitPerAgentConversation(agentIDs, pathFn); err != nil {
			log.Fatalf("main", "init conversation log: %v", err)
		}
		cleanups = append(cleanups, log.CloseConversation)
	}

	return func() {
		// Run cleanups in reverse order (like stacked defers)
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

// migrateConversationDB renames a legacy shared conversation.db to a per-agent
// file for the first configured agent. Runs once; skips if already migrated.
func migrateConversationDB(cfg *config.Config) {
	oldPath := cfg.Logging.ConversationFile
	if _, err := os.Stat(oldPath); err != nil {
		return // no old file
	}
	if len(cfg.Agents) == 0 {
		return
	}
	// Migrate to the first agent's per-agent path.
	firstID := cfg.Agents[0].ID
	newPath := sqlite.AgentPath(oldPath, firstID)
	if _, err := os.Stat(newPath); err == nil {
		return // already migrated
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		log.Errorf("main", "migrate conversation.db → %s: %v", filepath.Base(newPath), err)
		return
	}
	// Move WAL/SHM files too
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(oldPath+suffix, newPath+suffix)
	}
	log.Infof("main", "migrated %s → %s", filepath.Base(oldPath), filepath.Base(newPath))
}
