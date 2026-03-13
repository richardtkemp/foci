package config

import "strings"

// FieldType describes the expected value type for a settable config field.
type FieldType int

const (
	FieldString   FieldType = iota // bare string, quoted in TOML
	FieldInt                       // integer
	FieldFloat                     // float64
	FieldBool                      // true/false
	FieldDuration                  // Go duration string (e.g. "5m"), quoted in TOML
)

// ConfigField describes a single settable config key.
type ConfigField struct {
	Section     string    // TOML section: "defaults", "sessions", etc.
	Key         string    // TOML key within the section
	Type        FieldType // value type
	Description string    // one-line description
}

// Fields returns the registry of settable config fields.
// This is a curated list of scalar fields that can be set via /config set.
// Array, map, and complex nested fields are excluded.
func Fields() []ConfigField {
	return configFields
}

// LookupField finds a field by "section.key" (case-insensitive).
func LookupField(sectionKey string) (ConfigField, bool) {
	lower := strings.ToLower(sectionKey)
	for _, f := range configFields {
		if strings.ToLower(f.Section+"."+f.Key) == lower {
			return f, true
		}
	}
	return ConfigField{}, false
}

// FieldSections returns the distinct section names in registry order.
func FieldSections() []string {
	seen := map[string]bool{}
	var sections []string
	for _, f := range configFields {
		if !seen[f.Section] {
			seen[f.Section] = true
			sections = append(sections, f.Section)
		}
	}
	return sections
}

// FieldsInSection returns all fields whose Section matches (case-insensitive).
func FieldsInSection(section string) []ConfigField {
	lower := strings.ToLower(section)
	var result []ConfigField
	for _, f := range configFields {
		if strings.ToLower(f.Section) == lower {
			result = append(result, f)
		}
	}
	return result
}

var configFields = []ConfigField{
	// llm — global LLM settings inherited by agents
	{"llm", "model", FieldString, "default model for all agents"},
	{"llm", "max_output_tokens", FieldInt, "max tokens in model response"},

	// defaults — global defaults inherited by agents
	{"defaults", "max_tool_loops", FieldInt, "max tool iterations per turn"},
	{"defaults", "duplicate_messages", FieldBool, "send user text twice per API call"},
	{"defaults", "inject_agent_warnings", FieldBool, "inject warnings into agent session"},
	{"defaults", "show_tool_calls", FieldString, "tool call display: off, preview, full"},
	{"defaults", "show_thinking", FieldString, "thinking display: off, compact, true"},
	{"defaults", "steer_mode", FieldBool, "inject user messages between tool calls"},
	{"defaults", "stream_output", FieldBool, "stream model output to Telegram"},
	{"defaults", "stream_update_interval", FieldDuration, "interval between stream edits"},
	{"defaults", "braindead_threshold", FieldInt, "consecutive tool loops before warning (0=disabled)"},
	{"defaults", "nudge_enable", FieldBool, "enable mid-turn behavioral reminders"},
	{"defaults", "nudge_auto_extract", FieldBool, "auto-extract rules from character files via LLM"},
	{"defaults", "nudge_cooldown", FieldInt, "min tool calls between repeating same reminder"},
	{"defaults", "nudge_max_per_batch", FieldInt, "max reminders injected per tool batch"},
	{"defaults", "nudge_pre_answer_gate", FieldBool, "enable pre-answer verification gate"},
	{"defaults", "nudge_pre_answer_min_tools", FieldInt, "min tool calls before pre-answer gate fires"},
	{"defaults", "compaction_effort", FieldString, "effort for compaction API calls"},
	{"defaults", "max_result_chars", FieldInt, "max chars before writing result to file"},
	{"defaults", "auto_summarise", FieldBool, "auto-summarise oversized tool results"},
	{"defaults", "search_provider", FieldString, "web search: brave or anthropic"},
	{"defaults", "fetch_provider", FieldString, "web fetch: anthropic or builtin"},

	// agent — per-agent fields (written to [[agents]] block)
	{"agent", "model", FieldString, "model for this agent"},
	{"agent", "max_tool_loops", FieldInt, "max tool iterations per turn"},
	{"agent", "max_output_tokens", FieldInt, "max tokens in model response"},
	{"agent", "effort", FieldString, "effort level: low, medium, high"},
	{"agent", "thinking", FieldString, "thinking mode: adaptive or off"},
	{"agent", "duplicate_messages", FieldBool, "send user text twice per API call"},
	{"agent", "steer_mode", FieldBool, "inject user messages between tool calls"},
	{"agent", "nudge_enable", FieldBool, "enable mid-turn behavioral reminders"},
	{"agent", "nudge_auto_extract", FieldBool, "auto-extract rules from character files via LLM"},
	{"agent", "nudge_cooldown", FieldInt, "min tool calls between repeating same reminder"},
	{"agent", "nudge_max_per_batch", FieldInt, "max reminders injected per tool batch"},
	{"agent", "nudge_pre_answer_gate", FieldBool, "enable pre-answer verification gate"},
	{"agent", "nudge_pre_answer_min_tools", FieldInt, "min tool calls before pre-answer gate fires"},
	{"agent", "inject_agent_warnings", FieldBool, "inject warnings into agent session"},
	{"agent", "show_tool_calls", FieldString, "tool call display: off, preview, full"},
	{"agent", "show_thinking", FieldString, "thinking display: off, compact, true"},
	{"agent", "compaction_effort", FieldString, "effort for compaction API calls"},
	{"agent", "exec_auto_background", FieldInt, "seconds before auto-backgrounding exec"},
	{"agent", "max_result_chars", FieldInt, "max chars before writing result to file"},
	{"agent", "auto_summarise", FieldBool, "auto-summarise oversized tool results"},
	{"agent", "search_provider", FieldString, "web search: brave or anthropic"},
	{"agent", "fetch_provider", FieldString, "web fetch: anthropic or builtin"},
	{"agent", "tts", FieldString, "TTS provider id"},
	{"agent", "stt", FieldString, "STT provider id"},
	{"agent", "tts_rate", FieldFloat, "TTS speech rate multiplier"},
	{"agent", "received_files_dir", FieldString, "save received files to this directory"},
	{"agent", "display_width", FieldInt, "display width for dividers in Telegram"},
	{"agent", "table_wrap_lines", FieldInt, "max wrapped lines per table cell"},
	{"agent", "table_style", FieldString, "table style: pretty or markdown"},
	{"agent", "keepalive.enabled", FieldBool, "enable keepalive timer"},
	{"agent", "keepalive.interval", FieldDuration, "keepalive interval"},
	{"agent", "background.enabled", FieldBool, "enable background work timer"},
	{"agent", "background.interval", FieldDuration, "background interval"},
	{"agent", "memory_formation.interval", FieldDuration, "memory capture interval"},
	{"agent", "memory_formation.consolidation_interval", FieldDuration, "memory consolidation interval"},

	// anthropic
	{"anthropic", "effort", FieldString, "effort level: low, medium, high"},
	{"anthropic", "thinking", FieldString, "thinking mode: adaptive or off"},
	{"anthropic", "streaming", FieldBool, "use streaming API"},
	{"anthropic", "http_timeout", FieldDuration, "HTTP timeout for API calls"},

	// gemini
	{"gemini", "thinking", FieldString, "thinking mode: adaptive or off"},
	{"gemini", "http_timeout", FieldDuration, "HTTP timeout for API calls"},
	{"gemini", "cache_ttl", FieldDuration, "context cache TTL"},

	// sessions
	{"sessions", "compaction_threshold", FieldFloat, "compact at this fraction of context window"},
	{"sessions", "compaction_max_tokens", FieldInt, "max output tokens for summary"},
	{"sessions", "compaction_min_messages", FieldInt, "min messages before compacting"},
	{"sessions", "compaction_preserve_messages", FieldInt, "preserve last N messages through compaction"},
	{"sessions", "max_system_prompt_chars_file", FieldInt, "per-file char warning threshold"},
	{"sessions", "max_system_prompt_chars_total", FieldInt, "total system prompt char warning threshold"},

	// telegram
	{"telegram", "enable_startup_notify", FieldBool, "send notification on startup"},
	{"telegram", "enable_stop_aliases", FieldBool, "enable stop command aliases"},
	{"telegram", "multiball_session_ttl", FieldDuration, "idle TTL before multiball reclaim"},
	{"telegram", "message_queue_size", FieldInt, "outbound message queue buffer size"},
	{"telegram", "display_width", FieldInt, "display width for dividers"},
	{"telegram", "table_wrap_lines", FieldInt, "max wrapped lines per table cell"},
	{"telegram", "table_style", FieldString, "table style: pretty or markdown"},

	// tools
	{"tools", "max_result_chars", FieldInt, "max chars before writing result to file"},
	{"tools", "exec_auto_background", FieldInt, "seconds before auto-backgrounding exec"},
	{"tools", "exec_default_timeout", FieldInt, "default timeout for exec in seconds"},
	{"tools", "auto_summarise", FieldBool, "auto-summarise oversized results"},
	{"tools", "tmux_autopilot", FieldBool, "auto-unwatch on inactivity"},
	{"tools", "tmux_watch_threshold", FieldDuration, "default watch threshold duration"},
	{"tools", "tmux_session_ttl", FieldDuration, "auto-kill idle tmux sessions after"},
	{"tools", "tmux_cols", FieldInt, "tmux window columns"},
	{"tools", "tmux_rows", FieldInt, "tmux window rows"},
	{"tools", "max_concurrent_spawns", FieldInt, "max concurrent spawn sessions"},
	{"tools", "explore_max_depth", FieldInt, "max tool loops for explore spawn"},
	{"tools", "search_provider", FieldString, "web search: brave or anthropic"},
	{"tools", "fetch_provider", FieldString, "web fetch: anthropic or builtin"},
	{"tools", "web_fetch_timeout", FieldDuration, "HTTP timeout for web fetch"},
	{"tools", "web_search_timeout", FieldDuration, "HTTP timeout for web search"},
	{"tools", "tool_call_preview_chars", FieldInt, "max chars for tool call preview"},

	// logging
	{"logging", "level", FieldString, "log level: DEBUG, INFO, WARN, ERROR"},
	{"logging", "messages_in_log", FieldBool, "log user message content"},
	{"logging", "full_payload", FieldBool, "write full API payloads to file"},
	{"logging", "cache_bust_detect", FieldBool, "alert on cache_read drop"},
	{"logging", "log_rotation", FieldBool, "enable built-in log rotation"},
	{"logging", "rotation_period", FieldDuration, "how often to rotate logs"},
	{"logging", "retention_period", FieldDuration, "keep lines newer than this"},

	// memory
	{"memory", "reindex_debounce", FieldDuration, "delay before reindex"},
	{"memory", "conversation_weight", FieldFloat, "weight for conversation search results"},
	{"memory", "search_limit", FieldInt, "max search results to return"},
	{"memory", "sweep_interval", FieldDuration, "periodic full reindex interval"},

	// keepalive (global)
	{"keepalive", "enabled", FieldBool, "enable keepalive timer"},
	{"keepalive", "interval", FieldDuration, "time since cache last warmed"},

	// background (global)
	{"background", "enabled", FieldBool, "enable background work timer"},
	{"background", "interval", FieldDuration, "time since last interaction before firing"},

	// mana (global)
	{"mana", "invest_interval", FieldDuration, "quiet period after mana reset"},

	// memory_formation (global)
	{"memory_formation", "interval", FieldDuration, "time between captures"},
	{"memory_formation", "consolidation_interval", FieldDuration, "min time between consolidations"},

	// environment
	{"environment", "enabled", FieldBool, "inject environment block"},
	{"environment", "docs_path", FieldString, "path to platform docs directory"},

	// cache
	{"cache", "strategy", FieldString, "cache strategy: auto or explicit"},
	{"cache", "ttl", FieldString, "Anthropic prompt cache TTL: 5m or 1h"},

	// usage_warnings
	{"usage_warnings", "name", FieldString, "what to call quota (e.g. mana)"},
	{"usage_warnings", "restore_threshold", FieldInt, "mana restore notice threshold (0=disabled)"},

	// database
	{"database", "busy_timeout", FieldDuration, "SQLite busy timeout"},

	// debug
	{"debug", "log_api_key_suffix", FieldBool, "log last 4 chars of API keys on provider calls"},
	{"debug", "compaction_debug", FieldBool, "send compaction summary as Telegram file"},

	// http
	{"http", "port", FieldInt, "HTTP server port"},
	{"http", "bind", FieldString, "HTTP server bind address"},
}
