package main

import (
	"foci/internal/config"
	"foci/internal/log"
)

// missingSecret describes a secret that the config expects but the store lacks.
type missingSecret struct {
	ref         config.SecretRef
	downgraded  bool   // true if severity was downgraded from WARN to INFO
	explanation string // human-readable reason for downgrade (empty if not downgraded)
}

// warnMissingSecrets checks all secrets that the configuration expects to exist
// and emits a startup warning for each one that is missing from the store.
// Platform secrets (telegram/discord bots) are downgraded to INFO when the
// agent has at least one other working platform secret.
func warnMissingSecrets(cfg *config.Config, store config.SecretGetter) {
	for _, ms := range checkMissingSecrets(cfg, store) {
		if ms.downgraded {
			log.Infof("startup", "missing secret %q (needed by %s) — %s", ms.ref.Key, ms.ref.Context, ms.explanation)
		} else {
			log.Warnf("startup", "missing secret %q (needed by %s)", ms.ref.Key, ms.ref.Context)
		}
	}
}

// checkMissingSecrets returns structured results for each missing secret,
// including whether the severity was downgraded.
func checkMissingSecrets(cfg *config.Config, store config.SecretGetter) []missingSecret {
	refs := config.RequiredSecrets(cfg)

	// First pass: for each agent, check if at least one platform secret exists.
	agentHasWorkingPlatform := make(map[string]bool)
	for _, ref := range refs {
		if ref.Platform && ref.AgentID != "" {
			if _, ok := store.Get(ref.Key); ok {
				agentHasWorkingPlatform[ref.AgentID] = true
			}
		}
	}

	// Second pass: collect missing secrets with appropriate severity.
	var results []missingSecret
	for _, ref := range refs {
		if _, ok := store.Get(ref.Key); ok {
			continue
		}
		ms := missingSecret{ref: ref}
		if ref.Platform && ref.AgentID != "" && agentHasWorkingPlatform[ref.AgentID] {
			ms.downgraded = true
			ms.explanation = "agent has another working platform"
		}
		results = append(results, ms)
	}
	return results
}
