package main

import (
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

	// Conversation log (per-agent SQLite databases in workspace .data)
	if cfg.Logging.ConversationFile != "" {
		// Build agent ID → workspace map for path resolution
		agentWorkspaces := make(map[string]string, len(cfg.Agents))
		var agentIDs []string
		for _, acfg := range cfg.Agents {
			agentIDs = append(agentIDs, acfg.ID)
			agentWorkspaces[acfg.ID] = acfg.Workspace
		}
		// Migrate conversation DBs from shared data_dir to workspace .data
		for _, acfg := range cfg.Agents {
			oldPath := sqlite.AgentPath(cfg.Logging.ConversationFile, acfg.ID)
			newPath := config.AgentDataPath(acfg.Workspace, "conversation.db")
			if migrated, err := sqlite.MigrateFile(oldPath, newPath); err != nil {
				log.Errorf("main", "migrate conversation db for %s: %v", acfg.ID, err)
			} else if migrated {
				log.Infof("main", "migrated %s → %s", oldPath, newPath)
			}
		}
		pathFn := func(agentID string) string {
			if ws, ok := agentWorkspaces[agentID]; ok {
				return config.AgentDataPath(ws, "conversation.db")
			}
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
