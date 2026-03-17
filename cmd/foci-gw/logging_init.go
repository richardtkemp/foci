package main

import (
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/log"
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

		// Archive all existing log content on startup so each process
		// lifetime begins with a clean log file.
		log.RotateOnce(log.RotationConfig{
			Retention:   0, // archive everything
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
		})

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
		var agentIDs []string
		for _, acfg := range cfg.Agents {
			agentIDs = append(agentIDs, acfg.ID)
		}
		pathFn := func(agentID string) string {
			for _, acfg := range cfg.Agents {
				if acfg.ID == agentID {
					return config.AgentDataPath(acfg.Workspace, "conversation.db")
				}
			}
			return ""
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
