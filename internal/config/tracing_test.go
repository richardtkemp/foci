package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadTracingConfig(t *testing.T) {
	// Proves that a [tracing] block decodes every field, and that unset
	// pointer/scalar fields resolve to their documented defaults.
	tests := []struct {
		name          string
		toml          string
		wantEnabled   bool
		wantEndpoint  string
		wantEnv       string
		wantContent   bool
		wantSysPrompt bool
		wantMaxBytes  int
		wantFlush     time.Duration
	}{
		{
			name: "fully specified",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[tracing]
enabled = true
endpoint = "http://127.0.0.1:3100/api/public/otel"
environment = "dev"
content = false
system_prompt = false
max_field_bytes = 4096
flush_timeout = "10s"
`,
			wantEnabled:   true,
			wantEndpoint:  "http://127.0.0.1:3100/api/public/otel",
			wantEnv:       "dev",
			wantContent:   false,
			wantSysPrompt: false,
			wantMaxBytes:  4096,
			wantFlush:     10 * time.Second,
		},
		{
			name: "defaults apply when unset",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[tracing]
enabled = true
endpoint = "http://127.0.0.1:3100/api/public/otel"
`,
			wantEnabled:   true,
			wantEndpoint:  "http://127.0.0.1:3100/api/public/otel",
			wantEnv:       "production",
			wantContent:   true,
			wantSysPrompt: true,
			wantMaxBytes:  2097152,
			wantFlush:     5 * time.Second,
		},
		{
			name: "section absent — off by default",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`,
			wantEnabled:   false,
			wantEndpoint:  "",
			wantEnv:       "production",
			wantContent:   true,
			wantSysPrompt: true,
			wantMaxBytes:  2097152,
			wantFlush:     5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			tr := cfg.Tracing
			if tr.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", tr.Enabled, tt.wantEnabled)
			}
			if tr.Endpoint != tt.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", tr.Endpoint, tt.wantEndpoint)
			}
			if tr.Environment != tt.wantEnv {
				t.Errorf("Environment = %q, want %q", tr.Environment, tt.wantEnv)
			}
			if got := tr.ContentEnabled(); got != tt.wantContent {
				t.Errorf("ContentEnabled() = %v, want %v", got, tt.wantContent)
			}
			if got := tr.SystemPromptEnabled(); got != tt.wantSysPrompt {
				t.Errorf("SystemPromptEnabled() = %v, want %v", got, tt.wantSysPrompt)
			}
			if tr.MaxFieldBytes != tt.wantMaxBytes {
				t.Errorf("MaxFieldBytes = %d, want %d", tr.MaxFieldBytes, tt.wantMaxBytes)
			}
			if got := tr.FlushTimeoutDuration(); got != tt.wantFlush {
				t.Errorf("FlushTimeoutDuration() = %v, want %v", got, tt.wantFlush)
			}
		})
	}
}

func TestTracingConfig_FlushTimeoutDuration_FallsBackOnBadValue(t *testing.T) {
	// Proves FlushTimeoutDuration falls back to 5s for empty or unparsable
	// values, rather than propagating a zero/garbage duration to callers that
	// don't go through Validate first.
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"empty", "", 5 * time.Second},
		{"invalid", "not-a-duration", 5 * time.Second},
		{"valid", "30s", 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := TracingConfig{FlushTimeout: tt.val}
			if got := tr.FlushTimeoutDuration(); got != tt.want {
				t.Errorf("FlushTimeoutDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateTracing(t *testing.T) {
	// Proves validateTracing rejects an enabled tracing config with a missing
	// or relative endpoint, or an unparsable flush_timeout, and accepts a
	// well-formed one. Disabled tracing skips validation entirely regardless
	// of how broken the rest of the section is.
	tests := []struct {
		name    string
		cfg     TracingConfig
		wantErr bool
	}{
		{
			name:    "disabled — no validation even with garbage fields",
			cfg:     TracingConfig{Enabled: false, Endpoint: "", MaxFieldBytes: -1, FlushTimeout: "garbage"},
			wantErr: false,
		},
		{
			name:    "enabled without endpoint",
			cfg:     TracingConfig{Enabled: true, Endpoint: "", MaxFieldBytes: 100},
			wantErr: true,
		},
		{
			name:    "enabled with relative endpoint",
			cfg:     TracingConfig{Enabled: true, Endpoint: "127.0.0.1:3100/api/public/otel", MaxFieldBytes: 100},
			wantErr: true,
		},
		{
			name:    "enabled with non-http(s) scheme",
			cfg:     TracingConfig{Enabled: true, Endpoint: "ftp://example.com/otel", MaxFieldBytes: 100},
			wantErr: true,
		},
		{
			name:    "enabled with bad flush_timeout",
			cfg:     TracingConfig{Enabled: true, Endpoint: "http://127.0.0.1:3100/api/public/otel", FlushTimeout: "not-a-duration", MaxFieldBytes: 100},
			wantErr: true,
		},
		{
			name:    "enabled with non-positive max_field_bytes",
			cfg:     TracingConfig{Enabled: true, Endpoint: "http://127.0.0.1:3100/api/public/otel", MaxFieldBytes: 0},
			wantErr: true,
		},
		{
			name:    "enabled and well-formed",
			cfg:     TracingConfig{Enabled: true, Endpoint: "http://127.0.0.1:3100/api/public/otel", FlushTimeout: "5s", MaxFieldBytes: 2097152},
			wantErr: false,
		},
		{
			name:    "enabled and well-formed https",
			cfg:     TracingConfig{Enabled: true, Endpoint: "https://cloud.langfuse.com/api/public/otel", MaxFieldBytes: 2097152},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Tracing: tt.cfg}
			err := cfg.validateTracing()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTracing() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequiredSecretsTracing(t *testing.T) {
	// Proves the langfuse OTLP basic-auth secret refs are required exactly
	// when [tracing] is enabled, and absent otherwise.
	t.Run("enabled — both langfuse keys required", func(t *testing.T) {
		cfg := Config{Tracing: TracingConfig{Enabled: true, Endpoint: "http://127.0.0.1:3100/api/public/otel"}}
		refs := RequiredSecrets(&cfg)

		pub := assertHasKeyReturn(t, refs, "langfuse.public_key")
		if pub.Optional {
			t.Error("langfuse.public_key should not be Optional — a tracing config without keys is a misconfiguration")
		}
		sec := assertHasKeyReturn(t, refs, "langfuse.secret_key")
		if sec.Optional {
			t.Error("langfuse.secret_key should not be Optional")
		}
	})

	t.Run("disabled — no langfuse refs", func(t *testing.T) {
		cfg := Config{Tracing: TracingConfig{Enabled: false}}
		refs := RequiredSecrets(&cfg)
		for _, ref := range refs {
			if ref.Key == "langfuse.public_key" || ref.Key == "langfuse.secret_key" {
				t.Errorf("did not expect %q when tracing is disabled", ref.Key)
			}
		}
	})
}
