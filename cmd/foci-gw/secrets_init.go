package main

import (
	"os"
	"path/filepath"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
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
// sets up bitwarden (if enabled), and seeds default prompt files.
func initSecrets(configPath string, cfg *config.Config) secretsResult {
	// Seed default prompts and skills to ~/shared/ for user customisation
	if home, err := os.UserHomeDir(); err == nil {
		shared := filepath.Join(home, "shared")
		seedDefaultPrompts(filepath.Join(shared, "prompts"))
		seedDefaultSkills(filepath.Join(shared, "skills"))
	}

	// Load secrets (from secrets.toml alongside config file)
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		log.Fatalf("main", "load secrets: %v", err)
	}
	if names := store.Names(); len(names) > 0 {
		log.Infof("main", "loaded %d secrets: %v", len(names), names)
	}

	// Startup security checks for secrets.toml
	if !cfg.SkipSecurityChecks {
		if warnings := store.CheckSecurity(); len(warnings) > 0 {
			for _, w := range warnings {
				log.Warnf("security", "%s", w)
			}
		}
	}
	if len(cfg.Agents) > 1 && !store.HasAgentRestrictions() {
		log.Warnf("security", "multiple agents but no allowed_agents/denied_agents in secrets.toml — all agents can access all secrets")
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
		log.Infof("main", "generated HTTP API key — add FOCI_API_KEY to crontab: %s", httpAPIKey)
	}

	// Wire child process group-dropping into the command package
	// (so script commands also drop supplementary groups).
	command.ChildSysProcAttr = tools.ChildSysProcAttr

	// Bitwarden store (optional)
	var bwStore *bitwarden.Store
	var cleanup func()
	if cfg.Bitwarden.Enabled {
		secretTTL, _ := time.ParseDuration(cfg.Bitwarden.SecretTTL)
		bwExec := &bitwarden.DefaultExecutor{SessionFile: cfg.Bitwarden.SessionFile}
		bwStore = bitwarden.New(bwExec, secretTTL)

		if err := bwStore.Refresh(); err != nil {
			log.Errorf("main", "bitwarden initial refresh: %v", err)
		} else {
			log.Infof("main", "bitwarden: loaded %d vault items", bwStore.ItemCount())
		}

		// Background refresh ticker
		refreshInterval, _ := time.ParseDuration(cfg.Bitwarden.RefreshInterval)
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := bwStore.Refresh(); err != nil {
					log.Warnf("bitwarden", "background refresh: %v", err)
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
