package main

import (
	"foci/internal/config"
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
			startupLog.Infof("missing secret %q (needed by %s) — %s", ms.ref.Key, ms.ref.Context, ms.explanation)
		} else {
			startupLog.Warnf("missing secret %q (needed by %s)", ms.ref.Key, ms.ref.Context)
		}
	}
}

// checkMissingSecrets returns structured results for each missing secret,
// including whether the severity was downgraded.
func checkMissingSecrets(cfg *config.Config, store config.SecretGetter) []missingSecret {
	refs := config.RequiredSecrets(cfg)

	// The app platform (Android client) is a per-agent reachability channel that
	// carries no per-agent secret in the store — it's device-pairing based. When
	// it's enabled, every agent has a working channel even without a
	// telegram/discord bot token, so a missing tg/discord bot secret is INFO, not
	// WARN. (Checked at config level, not via app.Enabled() or "is a client
	// paired": this runs at startup before the app hub is built — app.Enabled()
	// is activeHub != nil, still false here — and before any client connects. The
	// [[platforms]] "app" entry is what enables the provider, so the config entry
	// is the correct, init-order-safe signal.)
	appEnabled := false
	for _, p := range cfg.Platforms {
		if p.ID == "app" {
			appEnabled = true
			break
		}
	}

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
		switch {
		case ref.Optional:
			// Best-effort feature: absence just disables it (e.g. brave search
			// self-gates on the key at runtime). INFO, not WARN.
			ms.downgraded = true
			ms.explanation = "optional — feature stays disabled until set"
		case ref.Platform && ref.AgentID != "" && agentHasWorkingPlatform[ref.AgentID]:
			ms.downgraded = true
			ms.explanation = "agent has another working platform"
		case ref.Platform && ref.AgentID != "" && appEnabled:
			ms.downgraded = true
			ms.explanation = "agent reachable via the app platform"
		}
		results = append(results, ms)
	}
	return results
}
