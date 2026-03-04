package main

import (
	"path/filepath"
	"time"

	"foci/config"
	"foci/log"
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

	// Conversation log (SQLite)
	if cfg.Logging.ConversationFile != "" {
		if err := log.InitConversation(cfg.Logging.ConversationFile); err != nil {
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
