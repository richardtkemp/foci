package main

import (
	"path/filepath"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/tools"
)

type secretsResult struct {
	store      *secrets.Store
	bwStore    *bitwarden.Store
	httpAPIKey string
	cleanup    func()
}

// initSecrets loads secrets.toml, runs security checks, generates the HTTP API key,
// and sets up bitwarden (if enabled).
func initSecrets(configPath string, cfg *config.Config) secretsResult {
	// Load secrets (from secrets.toml alongside config file)
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		log.Fatalf("main", "load secrets: %v", err)
	}
	if names := store.Names(); len(names) > 0 {
		mainLog.Infof("loaded %d secrets: %v", len(names), names)
	}

	// Startup security checks for secrets.toml
	if !cfg.SkipSecurityChecks {
		if warnings := store.CheckSecurity(); len(warnings) > 0 {
			for _, w := range warnings {
				securityLog.Warnf("%s", w)
			}
		}
	}
	if len(cfg.Agents) > 1 && !store.HasAgentRestrictions() {
		securityLog.Warnf("multiple agents but no allowed_agents/denied_agents in secrets.toml — all agents can access all secrets")
	}

	// On a host that has opted out of the strict secrets posture, let the HTTP
	// tools reach loopback targets (e.g. local test servers). Every other SSRF
	// block — private ranges, cloud-metadata, ULA — stays strict. Production
	// leaves skip_security_checks unset and keeps loopback blocked.
	if cfg.SkipSecurityChecks {
		tools.PermitLoopbackHTTP()
		securityLog.Warnf("skip_security_checks set — SSRF guard now permits loopback HTTP targets (dev/test only)")
	}

	// Auto-generate HTTP API key if not present
	httpAPIKey, _ := store.Get("http.api_key")
	if httpAPIKey == "" {
		generated, err := secrets.GeneratePassphrase(5)
		if err != nil {
			log.Fatalf("main", "generate HTTP API key: %v", err)
		}
		store.Set("http.api_key", generated)
		if err := store.Save(); err != nil {
			log.Fatalf("main", "save HTTP API key: %v", err)
		}
		httpAPIKey = generated
		mainLog.Infof("generated HTTP API key (for remote/cross-user access): %s", httpAPIKey)
	}

	// The app provider no longer uses a persisted shared "master" key (#862):
	// devices authenticate with per-device tokens, minted by exchanging a
	// single-use, in-memory pairing key (the /android wizard or `foci app
	// pair-key`) at POST /app/pair. So there is nothing to auto-generate here —
	// [[platforms]] id="app" brings the endpoint up with no shared secret.

	// Initialise the child-credential drop (probes CAP_SETGID, stashes a
	// Credential that filters foci-secrets out of child processes' groups).
	// Every subprocess foci-gw spawns goes through procx.Spawn /
	// procx.SpawnSetsid, which read the credential populated here.
	// Only foci-gw calls this — see internal/procx/procx.go for why the
	// foci CLI deliberately skips it (TODO #755 cron-log noise fix).
	if err := procx.Setup(); err != nil {
		if cfg.SkipSecurityChecks {
			securityLog.Warnf("procx child-credential setup failed but skip_security_checks is set — continuing INSECURELY (subprocesses keep the %s group): %v", procx.SecurityGroupName, err)
		} else {
			log.Fatalf("security", "procx child-credential setup failed: %v — subprocesses would inherit the %s group and could read secrets.toml. Fix CAP_SETGID (see docs/SECRETS.md) or set skip_security_checks=true to override.", err, procx.SecurityGroupName)
		}
	}

	// Bitwarden store (optional)
	var bwStore *bitwarden.Store
	var cleanup func()
	if cfg.Bitwarden.Enabled {
		secretTTL, _ := time.ParseDuration(cfg.Bitwarden.SecretTTL)
		bwExec := &bitwarden.DefaultExecutor{SessionFile: cfg.Bitwarden.SessionFile}
		bwStore = bitwarden.New(bwExec, secretTTL)

		if err := bwStore.Refresh(); err != nil {
			mainLog.Errorf("bitwarden initial refresh: %v", err)
		} else {
			mainLog.Infof("bitwarden: loaded %d vault items", bwStore.ItemCount())
		}

		// Background refresh ticker
		refreshInterval, _ := time.ParseDuration(cfg.Bitwarden.RefreshInterval)
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := bwStore.Refresh(); err != nil {
					bitwardenLog.Warnf("background refresh: %v", err)
				}
			}
		}()

		// Background cleanup of expired values
		cleanupInterval, _ := time.ParseDuration(cfg.Bitwarden.CleanupInterval)
		bwStore.StartCleanup(cleanupInterval)
		cleanup = bwStore.Close
	}

	return secretsResult{
		store:      store,
		bwStore:    bwStore,
		httpAPIKey: httpAPIKey,
		cleanup:    cleanup,
	}
}
