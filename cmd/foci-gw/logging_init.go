package main

import (
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/convo"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/tempdir"
)

// initLogging sets up event logging, log rotation, API DB, and conversation DB.
// processStart is the cutoff for the startup rotation pass.
// Returns a cleanup function that should be deferred.
func initLogging(cfg *config.Config, processStart time.Time) func() {
	logFileMode, err := config.ParseFileMode(cfg.Logging.LogFileMode)
	if err != nil {
		log.Fatalf("main", "parse log_file_mode: %v", err)
	}

	if err := log.Init(log.Config{
		Level:       cfg.Logging.Level,
		EventFile:   cfg.Logging.EventFile,
		APIFile:     cfg.Logging.APIFile,
		PayloadFile: cfg.Logging.PayloadFile,
		FullPayload: cfg.Logging.FullPayload,
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
			mainLog.Infof("extra logging enabled for %q (grep xtra:%s)", pkg, pkg)
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
				rotateLog.Warnf("temp cleanup: %v", err)
			} else if n > 0 {
				rotateLog.Infof("cleaned %d stale spawn-result files", n)
			}
		}

		// Archive every earlier process lifetime's content on startup so each
		// lifetime begins with a clean log file. The cutoff is this process's
		// start, NOT "now": the early log.Init in main has been writing to the
		// event file since before config load, and archiving by "now" swept
		// this lifetime's opening lines (config warnings, shellenv, preload)
		// into the archive, leaving the live log to begin at the rotate line
		// (#1869).
		log.RotateOnce(log.RotationConfig{
			Before:      processStart,
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

	// The cost ledger (#2111) in api.db. Opening a pre-ledger api.db migrates
	// it — once, under a VACUUM INTO backup, in one transaction — so the
	// cutover happens at the first start of a binary that books through it.
	if cfg.Logging.APIDB != "" {
		ledger, report, err := accounting.Open(cfg.Logging.APIDB, accounting.Options{
			// Invariants alarm and are never absorbed (#2111 R9). They are
			// WARNs until the P3 checks route them to operator chat.
			OnAlarm: func(a accounting.Alarm) {
				ledgerLog.Warnf("%s [%s]: %s", a.Invariant, a.Backend, a.Detail)
			},
		})
		if err != nil {
			log.Fatalf("main", "open cost ledger %s: %v", cfg.Logging.APIDB, err)
		}
		if report != nil {
			ledgerLog.Infof("migrated the pre-ledger api.db (backup %s):\n%s", report.BackupPath, report.String())
		}
		accounting.SetLive(ledger)
		cleanups = append(cleanups, func() {
			accounting.SetLive(nil)
			_ = ledger.Close()
		})
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
		if err := convo.InitPerAgent(agentIDs, pathFn); err != nil {
			log.Fatalf("main", "init conversation log: %v", err)
		}
		cleanups = append(cleanups, convo.Close)
	}

	return func() {
		// Run cleanups in reverse order (like stacked defers)
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}
