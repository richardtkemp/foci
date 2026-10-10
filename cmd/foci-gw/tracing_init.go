package main

import (
	"context"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/telemetry"
)

var tracingLog = log.NewComponentLogger("tracing")

// initTracing arms OpenTelemetry export of every turn ([tracing] in
// foci.toml). Returns the flush-and-stop cleanup. Runs after secrets are
// loaded. The exporter's basic-auth pair (langfuse.public_key /
// langfuse.secret_key) is read once HERE — changing it needs a restart. The
// REDACTION values are live instead: telemetry calls the root store per
// exported field and accumulates what it sees, so a secret added, changed
// or overridden in an [agents.<id>.*] table after startup — or removed from
// the file — is scrubbed from traces without a restart.
func initTracing(ctx context.Context, cfg *config.Config, store *secrets.Store) func() {
	if !cfg.Tracing.Enabled {
		return func() {}
	}
	pk, _ := store.Get("langfuse.public_key")
	sk, _ := store.Get("langfuse.secret_key")
	err := telemetry.Init(ctx, telemetry.Options{
		Endpoint:       cfg.Tracing.Endpoint,
		PublicKey:      pk,
		SecretKey:      sk,
		Environment:    cfg.Tracing.Environment,
		Content:        cfg.Tracing.ContentEnabled(),
		SystemPrompt:   cfg.Tracing.SystemPromptEnabled(),
		MaxFieldBytes:  cfg.Tracing.MaxFieldBytes,
		FlushTimeout:   cfg.Tracing.FlushTimeoutDuration(),
		ServiceVersion: version,
		SecretValues:   store.RedactionValues,
	})
	if err != nil {
		// Misconfiguration disables tracing; it never stops the gateway.
		tracingLog.Errorf("tracing disabled: %v", err)
		return func() {}
	}
	return func() { telemetry.Shutdown(context.Background()) }
}
