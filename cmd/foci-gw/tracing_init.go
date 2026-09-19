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
// loaded because the exporter's basic-auth pair and the redaction list both
// come from the store: the gateway scrubs the actual secret VALUES out of
// every exported text field, so the store's whole value set is handed over —
// including the Langfuse keys themselves.
func initTracing(ctx context.Context, cfg *config.Config, store *secrets.Store) func() {
	if !cfg.Tracing.Enabled {
		return func() {}
	}
	pk, _ := store.Get("langfuse.public_key")
	sk, _ := store.Get("langfuse.secret_key")
	var values []string
	for _, name := range store.Names() {
		if v, ok := store.Get(name); ok {
			values = append(values, v)
		}
	}
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
		SecretValues:   values,
	})
	if err != nil {
		// Misconfiguration disables tracing; it never stops the gateway.
		tracingLog.Errorf("tracing disabled: %v", err)
		return func() {}
	}
	return func() { telemetry.Shutdown(context.Background()) }
}
