package main

import (
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/convo"
	"foci/internal/delegator"
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
			// Invariants alarm and are never absorbed (#2111 R9): each goes
			// to operator chat through the shared expectation guard (#2013),
			// at ERROR, rate-limited per backend and invariant.
			OnAlarm: func(a accounting.Alarm) {
				delegator.Expectations.Violated(ledgerLog, a.Backend, a.Version, "ledger "+a.Invariant, a.Detail)
			},
		})
		if err != nil {
			log.Fatalf("main", "open cost ledger %s: %v", cfg.Logging.APIDB, err)
		}
		if report != nil {
			ledgerLog.Infof("migrated the pre-ledger api.db (backup %s):\n%s", report.BackupPath, report.String())
		}
		// No backend process of an earlier gateway survives into this one, so
		// a turn it left marked running (a crash, or a shutdown mid-turn) has
		// stopped spending (#2111 R8).
		if n, err := ledger.CloseOrphanedTurns(time.Now()); err != nil {
			ledgerLog.Warnf("close orphaned turns: %v", err)
		} else if n > 0 {
			ledgerLog.Infof("closed the activity of %d turn(s) an earlier gateway left running", n)
		}
		accounting.SetLive(ledger)
		cleanups = append(cleanups, func() {
			accounting.SetLive(nil)
			_ = ledger.Close()
		})
	}

	// The shadow ledger (#2111 §12): a scratch api.db a backend's new cost
	// adapter books into beside the live path, for `foci-gw ledger-shadow` to
	// compare. Its alarms are INFO — they are the verification's findings,
	// not operator alarms — and a failure to open it never stops the gateway.
	if cfg.Logging.APIShadowDB != "" {
		shadow, _, err := accounting.Open(cfg.Logging.APIShadowDB, accounting.Options{
			Shadow: true, NoBackup: true,
			OnAlarm: func(a accounting.Alarm) {
				ledgerLog.Infof("shadow %s [%s]: %s", a.Invariant, a.Backend, a.Detail)
			},
		})
		if err != nil {
			ledgerLog.Warnf("open shadow ledger %s: %v — running without it", cfg.Logging.APIShadowDB, err)
		} else {
			ledgerLog.Infof("shadow ledger %s: open for an adapter under verification (no backend books here at present)", cfg.Logging.APIShadowDB)
			accounting.SetShadow(shadow)
			cleanups = append(cleanups, func() {
				accounting.SetShadow(nil)
				_ = shadow.Close()
			})
		}
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
