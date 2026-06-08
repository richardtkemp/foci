package main

import (
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/tempdir"
)

// initLogging sets up event logging, log rotation, API DB, and conversation DB.
// Returns a cleanup function that should be deferred.
func initLogging(cfg *config.Config) func() {
	logFileMode, err := config.ParseFileMode(cfg.Logging.LogFileMode)
	if err != nil {
		log.Fatalf("main", "parse log_file_mode: %v", err)
	}

	if err := log.Init(log.Config{
		Level:       cfg.Logging.Level,
		EventFile:   cfg.Logging.EventFile,
		APIFile:     cfg.Logging.APIFile,
		PayloadFile: cfg.Logging.PayloadFile,
		FileMode:    logFileMode,
	}); err != nil {
		log.Fatalf("main", "init logging: %v", err)
	}

	// Per-package "extra" verbose logging (top-level [debug] flags). Applied
	// once here, process-global: enabling a package emits its xtra:<pkg> lines
	// at INFO regardless of per-agent scope. Off by default.
	for pkg, on := range map[string]bool{
		"ccstream": config.DerefBool(cfg.Debug.ExtraCcstreamLogging),
		"telegram": config.DerefBool(cfg.Debug.ExtraTelegramLogging),
		"inbox":    config.DerefBool(cfg.Debug.ExtraInboxLogging),
	} {
		if on {
			log.EnableExtra(pkg)
			log.Infof("main", "extra logging enabled for %q (grep xtra:%s)", pkg, pkg)
		}
	}

	var cleanups []func()
	cleanups = append(cleanups, log.Close)

	// Log rotation
	if config.DerefBool(cfg.Logging.LogRotation) {
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
		cleanupTempFiles := func() {
			n, err := tempdir.CleanOldFiles(tempdir.Dir(), "spawn-result-*.txt", 7*24*time.Hour)
			if err != nil {
				log.Warnf("rotate", "temp cleanup: %v", err)
			} else if n > 0 {
				log.Infof("rotate", "cleaned %d stale spawn-result files", n)
			}
		}

		// Archive all existing log content on startup so each process
		// lifetime begins with a clean log file.
		log.RotateOnce(log.RotationConfig{
			Retention:   0, // archive everything
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
			FileMode:    logFileMode,
			PostRotate:  cleanupTempFiles,
		})

		stopRotation := log.StartRotation(log.RotationConfig{
			Period:      rotPeriod,
			Retention:   retPeriod,
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
			FileMode:    logFileMode,
			PostRotate:  cleanupTempFiles,
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
	if config.DerefBool(cfg.Logging.ConversationLog) {
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
