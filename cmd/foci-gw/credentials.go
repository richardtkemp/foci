package main

import (
	"context"
	"fmt"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/secrets"
)

// formatResolvers maps wire format names to custom CredentialResolver implementations.
// Formats without an entry fall back to simple API key resolution.
var formatResolvers = make(map[string]provider.CredentialResolver)

// initCredentialResolvers initializes the credential resolver registry.
// Currently registers the anthropic resolver.
func initCredentialResolvers(ctx context.Context, cfg *config.Config, store *secrets.Store) error {
	resolver, err := anthropic.NewResolver(ctx, &cfg.Anthropic, store)
	if err != nil {
		return fmt.Errorf("init anthropic resolver: %w", err)
	}
	formatResolvers["anthropic"] = resolver
	return nil
}

