package config

import "time"

// defaultTracingFlushTimeout is the fallback used by FlushTimeoutDuration when
// FlushTimeout is empty or fails to parse (matches the `default:"5s"` tag).
const defaultTracingFlushTimeout = 5 * time.Second

// TracingConfig configures OpenTelemetry trace export of every agent turn
// (root span + tool/subagent children + one generation per api.db row) to an
// OTLP/HTTP collector — in practice a self-hosted Langfuse. Off by default.
type TracingConfig struct {
	Enabled       bool   `toml:"enabled" desc:"Export a trace for every agent turn (tool calls, subagents, prompt/reply content, usage and cost) over OTLP/HTTP to the endpoint below"`
	Endpoint      string `toml:"endpoint" desc:"OTLP/HTTP base URL; spans are POSTed to <endpoint>/v1/traces. For self-hosted Langfuse: http://127.0.0.1:3100/api/public/otel"`
	Environment   string `toml:"environment" default:"production" desc:"Value of the langfuse.environment attribute stamped on every span (e.g. production, dev)"`
	Content       *bool  `toml:"content" default:"true" desc:"Attach prompt, reply, thinking and tool input/output text to spans (secret values are redacted before export); false exports shape, timing, usage and cost only"`
	SystemPrompt  *bool  `toml:"system_prompt" default:"true" desc:"Also export the full system prompt text as a child event the first time a session launches with a new prompt hash (the hash and length are always recorded)"`
	MaxFieldBytes int    `toml:"max_field_bytes" default:"2097152" desc:"Longest text field exported on a span before truncation, in bytes"`
	FlushTimeout  string `toml:"flush_timeout" default:"5s" type:"duration" desc:"How long shutdown waits for buffered spans to export"`
}

// ContentEnabled returns the resolved value (default: true).
func (t TracingConfig) ContentEnabled() bool {
	return DerefBoolDefault(t.Content, true)
}

// SystemPromptEnabled returns the resolved value (default: true).
func (t TracingConfig) SystemPromptEnabled() bool {
	return DerefBoolDefault(t.SystemPrompt, true)
}

// FlushTimeoutDuration parses FlushTimeout, falling back to 5s when empty or
// invalid (should not happen after Load applies the `default:"5s"` tag, but
// callers may hold a TracingConfig built without going through Load).
func (t TracingConfig) FlushTimeoutDuration() time.Duration {
	if t.FlushTimeout == "" {
		return defaultTracingFlushTimeout
	}
	d, err := time.ParseDuration(t.FlushTimeout)
	if err != nil {
		return defaultTracingFlushTimeout
	}
	return d
}
