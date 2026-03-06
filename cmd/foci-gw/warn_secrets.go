package main

import (
	"foci/internal/config"
	"foci/internal/log"
)

// warnMissingSecrets checks all secrets that the configuration expects to exist
// and emits a startup warning for each one that is missing from the store.
func warnMissingSecrets(cfg *config.Config, store config.SecretGetter) {
	for _, ref := range config.RequiredSecrets(cfg) {
		if _, ok := store.Get(ref.Key); !ok {
			log.Warnf("startup", "missing secret %q (needed by %s)", ref.Key, ref.Context)
		}
	}
}
