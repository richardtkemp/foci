package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

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

// Constraint defines validation rules for a config field value.
type Constraint struct {
	Min     *float64 // minimum (inclusive), for FieldInt/FieldFloat
	Max     *float64 // maximum (inclusive), for FieldInt/FieldFloat
	Choices []string // valid values, for FieldString (case-insensitive match)
}

func fptr(v float64) *float64 { return &v }

// fieldConstraints maps "section.key" to its constraint.
// Fields not in this map accept any value that passes type parsing.
var fieldConstraints = map[string]Constraint{
	// Float [0,1]
	"sessions.compaction_threshold":                    {Min: fptr(0), Max: fptr(1)},
	"sessions.autocompact_before_mana_refresh_preserve_pct": {Min: fptr(0), Max: fptr(1)},
	"memory.conversation_weight":   {Min: fptr(0), Max: fptr(1)},

	// Int ranges
	"http.port":                        {Min: fptr(1), Max: fptr(65535)},
	"usage_warnings.restore_threshold": {Min: fptr(0), Max: fptr(100)},

	// Int >= 0
	"sessions.compaction_max_tokens":       {Min: fptr(0)},
	"sessions.compaction_min_messages":     {Min: fptr(0)},
	"sessions.compaction_preserve_messages": {Min: fptr(0)},

	// String choices
	"logging.level":            {Choices: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
	"cache.strategy":           {Choices: []string{"auto", "explicit"}},
	"cache.ttl":                {Choices: []string{"5m", "1h"}},
	"platforms.show_tool_calls":        {Choices: []string{"off", "preview", "full"}},
	"platforms.show_thinking":          {Choices: []string{"off", "compact", "true"}},
	"platforms.telegram.table_style":   {Choices: []string{"pretty", "markdown"}},
	"tools.todo_format":        {Choices: []string{"lines", "table"}},
	"agent.defaults.show_tool_calls":    {Choices: []string{"off", "preview", "full"}},
	"agent.defaults.show_thinking":      {Choices: []string{"off", "compact", "true"}},
	"agent.tools.search_provider":    {Choices: []string{"brave", "anthropic"}},
	"agent.tools.fetch_provider":     {Choices: []string{"anthropic", "builtin"}},
	"agent.tools.todo_format":        {Choices: []string{"lines", "table"}},
	"tools.search_provider":    {Choices: []string{"brave", "anthropic"}},
	"tools.fetch_provider":     {Choices: []string{"anthropic", "builtin"}},
}

// GetConstraint returns the constraint for this field, or nil if unconstrained.
func (f ConfigField) GetConstraint() *Constraint {
	c, ok := fieldConstraints[f.Section+"."+f.Key]
	if !ok {
		return nil
	}
	return &c
}

// ValidateValue checks raw user input against this field's constraint.
// It returns nil if the value is acceptable (or the field has no constraint).
func (f ConfigField) ValidateValue(raw string) error {
	c := f.GetConstraint()
	if c == nil {
		return nil
	}

	if len(c.Choices) > 0 {
		lower := strings.ToLower(raw)
		for _, ch := range c.Choices {
			if strings.ToLower(ch) == lower {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(c.Choices, ", "))
	}

	// Numeric range check (works for both FieldInt and FieldFloat).
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil // type parsing will catch this separately
	}
	if c.Min != nil && v < *c.Min {
		return fmt.Errorf("must be >= %s", formatNum(*c.Min))
	}
	if c.Max != nil && v > *c.Max {
		return fmt.Errorf("must be <= %s", formatNum(*c.Max))
	}
	return nil
}

// ConstraintHint returns a human-readable hint string, e.g. "0–1" or "off, preview, full".
// Returns "" if the field has no constraint.
func (f ConfigField) ConstraintHint() string {
	c := f.GetConstraint()
	if c == nil {
		return ""
	}
	if len(c.Choices) > 0 {
		return strings.Join(c.Choices, ", ")
	}
	if c.Min != nil && c.Max != nil {
		return formatNum(*c.Min) + "–" + formatNum(*c.Max)
	}
	if c.Min != nil {
		return ">= " + formatNum(*c.Min)
	}
	if c.Max != nil {
		return "<= " + formatNum(*c.Max)
	}
	return ""
}

// formatNum formats a float64 as an integer string when possible.
func formatNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
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

// FieldSections returns the distinct section names in alphabetical order.
func FieldSections() []string {
	seen := map[string]bool{}
	var sections []string
	for _, f := range configFields {
		if !seen[f.Section] {
			seen[f.Section] = true
			sections = append(sections, f.Section)
		}
	}
	sort.Strings(sections)
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
	// defaults — global defaults inherited by agents
	{"defaults", "max_output_tokens", FieldInt, "max tokens in model response"},
	{"defaults", "max_tool_loops", FieldInt, "max tool iterations per turn"},
	{"defaults", "duplicate_messages", FieldBool, "send user text twice per API call"},
	{"defaults", "inject_agent_warnings", FieldString, "inject warnings into agent session: all, errors, off"},
	{"defaults", "inject_chat_warnings", FieldString, "send warnings as chat notifications: all, errors, off"},
	{"defaults", "startup_notify", FieldBool, "send notification on startup"},
	{"defaults", "compaction_notify", FieldBool, "send notification on compaction"},
	{"defaults", "task_list_notify", FieldBool, "send notification on task list changes"},
	{"defaults", "compaction_debug", FieldBool, "send compaction summary as file attachment"},
	{"platforms", "show_tool_calls", FieldString, "tool call display: off, preview, full"},
	{"platforms", "show_thinking", FieldString, "thinking display: off, compact, true"},
	{"defaults", "steer_mode", FieldBool, "inject user messages between tool calls"},
	{"platforms", "stream_output", FieldBool, "stream model output"},
	{"platforms", "stream_interval", FieldDuration, "interval between stream edits"},
	{"defaults", "nudge_default_braindead_threshold", FieldInt, "consecutive tool loops before warning (0=disabled)"},
	{"defaults", "nudge_enable", FieldBool, "enable mid-turn behavioral reminders"},
	{"defaults", "nudge_auto_extract", FieldBool, "auto-extract rules from character files via LLM"},
	{"defaults", "nudge_cooldown", FieldInt, "min tool calls between repeating same reminder"},
	{"defaults", "nudge_max_per_batch", FieldInt, "max reminders injected per tool batch"},
	{"defaults", "nudge_pre_answer_gate", FieldBool, "enable pre-answer verification gate"},
	{"defaults", "nudge_pre_answer_min_tools", FieldInt, "min tool calls before pre-answer gate fires"},
	{"defaults", "nudge_default_enable", FieldBool, "enable built-in tool/skill reminders"},
	{"defaults", "nudge_default_frequency", FieldInt, "turns between tool/skill reminders (default 50)"},
	{"defaults", "nudge_default_scratchpad_frequency", FieldInt, "turns between scratchpad review reminders (0=disabled, default 20)"},
	{"tools", "max_result_chars", FieldInt, "max chars before writing result to file"},
	{"tools", "auto_summarise", FieldBool, "auto-summarise oversized tool results"},
	{"tools", "search_provider", FieldString, "web search: brave or anthropic"},
	{"tools", "fetch_provider", FieldString, "web fetch: anthropic or builtin"},
	{"tools", "todo_format", FieldString, "todo list format: lines or table"},
	{"sessions", "facet_no_compact", FieldBool, "set no_compact on facet sessions (default true)"},

	// agent — per-agent fields (written to [[agents]] block)
	{"agent", "defaults.max_tool_loops", FieldInt, "max tool iterations per turn"},
	{"agent", "defaults.max_output_tokens", FieldInt, "max tokens in model response"},
	{"agent", "defaults.duplicate_messages", FieldBool, "send user text twice per API call"},
	{"agent", "defaults.steer_mode", FieldBool, "inject user messages between tool calls"},
	{"agent", "defaults.nudge_enable", FieldBool, "enable mid-turn behavioral reminders"},
	{"agent", "defaults.nudge_auto_extract", FieldBool, "auto-extract rules from character files via LLM"},
	{"agent", "defaults.nudge_cooldown", FieldInt, "min tool calls between repeating same reminder"},
	{"agent", "defaults.nudge_max_per_batch", FieldInt, "max reminders injected per tool batch"},
	{"agent", "defaults.nudge_pre_answer_gate", FieldBool, "enable pre-answer verification gate"},
	{"agent", "defaults.nudge_pre_answer_min_tools", FieldInt, "min tool calls before pre-answer gate fires"},
	{"agent", "defaults.nudge_default_enable", FieldBool, "enable built-in tool/skill reminders"},
	{"agent", "defaults.nudge_default_frequency", FieldInt, "turns between tool/skill reminders (default 50)"},
	{"agent", "defaults.nudge_default_scratchpad_frequency", FieldInt, "turns between scratchpad review reminders (0=disabled, default 20)"},
	{"agent", "defaults.inject_agent_warnings", FieldString, "inject warnings into agent session: all, errors, off"},
	{"agent", "defaults.inject_chat_warnings", FieldString, "send warnings as chat notifications: all, errors, off"},
	{"agent", "defaults.show_tool_calls", FieldString, "tool call display: off, preview, full"},
	{"agent", "defaults.show_thinking", FieldString, "thinking display: off, compact, true"},
	{"agent", "tools.exec_auto_background", FieldInt, "seconds before auto-backgrounding exec"},
	{"agent", "tools.max_result_chars", FieldInt, "max chars before writing result to file"},
	{"agent", "tools.auto_summarise", FieldBool, "auto-summarise oversized tool results"},
	{"agent", "tools.search_provider", FieldString, "web search: brave or anthropic"},
	{"agent", "tools.fetch_provider", FieldString, "web fetch: anthropic or builtin"},
	{"agent", "tools.todo_format", FieldString, "todo list format: lines or table"},
	{"agent", "sessions.facet_no_compact", FieldBool, "set no_compact on facet sessions (default true)"},
	{"agent", "defaults.tts", FieldString, "TTS provider id"},
	{"agent", "defaults.stt", FieldString, "STT provider id"},
	{"agent", "defaults.tts_rate", FieldFloat, "TTS speech rate multiplier"},
	{"agent", "keepalive.enabled", FieldBool, "enable keepalive timer"},
	{"agent", "keepalive.interval", FieldDuration, "keepalive interval"},
	{"agent", "background.enabled", FieldBool, "enable background work timer"},
	{"agent", "background.interval", FieldDuration, "background interval"},
	{"agent", "memory_formation.interval", FieldDuration, "memory capture interval"},
	{"agent", "memory_formation.consolidation_interval", FieldDuration, "memory consolidation interval"},
	{"agent", "memory_formation.compaction_enabled", FieldBool, "memory capture before compaction"},

	// anthropic
	{"anthropic", "streaming", FieldBool, "use streaming API"},
	{"anthropic", "http_timeout", FieldDuration, "HTTP timeout for API calls"},

	// gemini
	{"gemini", "http_timeout", FieldDuration, "HTTP timeout for API calls"},
	{"gemini", "cache_ttl", FieldDuration, "context cache TTL"},

	// sessions
	{"sessions", "compaction_threshold", FieldFloat, "compact at this fraction of context window"},
	{"sessions", "compaction_max_tokens", FieldInt, "max output tokens for summary"},
	{"sessions", "compaction_min_messages", FieldInt, "min messages before compacting"},
	{"sessions", "compaction_preserve_messages", FieldInt, "preserve last N messages through compaction"},
	{"sessions", "autocompact_before_mana_refresh_preserve_pct", FieldFloat, "fraction of messages to preserve in mana-refresh mode (0.0-1.0)"},
	{"sessions", "max_system_prompt_chars_file", FieldInt, "per-file char warning threshold"},
	{"sessions", "max_system_prompt_chars_total", FieldInt, "total system prompt char warning threshold"},

	// platforms
	{"platforms", "startup_notify", FieldBool, "send notification on startup"},

	// defaults — stop aliases
	{"defaults", "enable_stop_aliases", FieldBool, "enable stop command aliases"},
	{"platforms", "facet_session_ttl", FieldDuration, "idle TTL before facet reclaim"},
	{"platforms", "message_queue_size", FieldInt, "message queue buffer size"},
	{"platforms", "display_width", FieldInt, "display width for dividers"},
	{"platforms", "telegram.table_wrap_lines", FieldInt, "max wrapped lines per table cell"},
	{"platforms", "telegram.table_style", FieldString, "table style: pretty or markdown"},

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
	{"debug", "messages_in_log", FieldBool, "log user message content"},
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
	{"memory_formation", "compaction_enabled", FieldBool, "memory capture before compaction"},

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

	// http
	{"http", "port", FieldInt, "HTTP server port"},
	{"http", "bind", FieldString, "HTTP server bind address"},
}
