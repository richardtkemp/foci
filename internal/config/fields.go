package config

import (
	"fmt"
	"reflect"
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
	Section     string    // TOML section: "agent_loop", "sessions", etc.
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
	"platforms.display.show_tool_calls":        {Choices: []string{"off", "preview", "full"}},
	"platforms.display.show_thinking":          {Choices: []string{"off", "compact", "true"}},
	"platforms.telegram.table_style":   {Choices: []string{"pretty", "markdown"}},
	"tools.todo_format":        {Choices: []string{"lines", "table"}},
	"agent.display.show_tool_calls":     {Choices: []string{"off", "preview", "full"}},
	"agent.display.show_thinking":       {Choices: []string{"off", "compact", "true"}},
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

// ── Registry builder types ──────────────────────────────────────────

// scope describes where a config struct appears (global section, per-agent, per-platform).
type scope struct {
	section string // TOML section name for emitted ConfigField
	prefix  string // key prefix (e.g. "notify." for agent/platform scoped fields)
}

// fieldAnnotation provides optional metadata for a specific TOML key.
type fieldAnnotation struct {
	key  string
	desc string
	dur  bool // override *string → FieldDuration
}

// registration groups a struct type with its scopes and annotations.
type registration struct {
	structType  reflect.Type
	scopes      []scope
	annotations map[string]fieldAnnotation
}

// global emits fields with the given section name (e.g. "notify", "agent_loop").
func global(section string) scope { return scope{section: section} }

// agent emits fields with section "agent" and keys prefixed by the given name
// (e.g. agent("notify") → key "notify.startup_notify").
func agent(section string) scope { return scope{section: "agent", prefix: section + "."} }

// platform emits fields with section "platforms" and keys prefixed by the given name
// (e.g. platform("notify") → key "notify.startup_notify").
func platform(section string) scope { return scope{section: "platforms", prefix: section + "."} }

// desc provides a description for a field.
func desc(key, description string) fieldAnnotation { return fieldAnnotation{key: key, desc: description} }

// dur marks a *string field as FieldDuration and provides a description.
func dur(key, description string) fieldAnnotation { return fieldAnnotation{key: key, desc: description, dur: true} }

// reg creates a registration for a struct type with the given scopes and annotations.
func reg(structVal any, args ...any) registration {
	r := registration{
		structType:  reflect.TypeOf(structVal),
		annotations: make(map[string]fieldAnnotation),
	}
	for _, a := range args {
		switch v := a.(type) {
		case scope:
			r.scopes = append(r.scopes, v)
		case fieldAnnotation:
			r.annotations[v.key] = v
		}
	}
	return r
}

// buildRegistry generates ConfigField entries from declarative registrations.
func buildRegistry(regs ...registration) []ConfigField {
	var fields []ConfigField
	for _, r := range regs {
		discovered := walkStructFields(r.structType, "", r.annotations)
		for _, sc := range r.scopes {
			for _, d := range discovered {
				fields = append(fields, ConfigField{
					Section:     sc.section,
					Key:         sc.prefix + d.key,
					Type:        d.fieldType,
					Description: d.desc,
				})
			}
		}
	}
	return fields
}

// discoveredField is a field found by reflection.
type discoveredField struct {
	key       string
	fieldType FieldType
	desc      string
}

// walkStructFields reflects on a struct type and returns all settable fields.
func walkStructFields(t reflect.Type, prefix string, annotations map[string]fieldAnnotation) []discoveredField {
	var result []discoveredField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		ft := sf.Type

		// Embedded struct (anonymous field) — recurse with same prefix.
		// Must check before tag parsing since embeds often have no TOML tag.
		if sf.Anonymous && ft.Kind() == reflect.Struct {
			result = append(result, walkStructFields(ft, prefix, annotations)...)
			continue
		}

		tag := sf.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip TOML tag options (e.g. ",omitempty" → strip).
		if idx := strings.Index(tag, ","); idx >= 0 {
			tag = tag[:idx]
			if tag == "" {
				continue
			}
		}

		key := prefix + tag

		// Pointer to struct — recurse with key prefix (e.g. *TelegramSpecific).
		if ft.Kind() == reflect.Ptr && ft.Elem().Kind() == reflect.Struct {
			result = append(result, walkStructFields(ft.Elem(), key+".", annotations)...)
			continue
		}

		// Named struct field (non-pointer, non-anonymous) — recurse with key prefix.
		if ft.Kind() == reflect.Struct {
			result = append(result, walkStructFields(ft, key+".", annotations)...)
			continue
		}

		// Determine FieldType from Go type.
		fieldType, ok := inferFieldType(ft)
		if !ok {
			continue // skip slices, maps, etc.
		}

		// Check annotations for description and duration override.
		var description string
		if ann, found := annotations[tag]; found {
			description = ann.desc
			if ann.dur {
				fieldType = FieldDuration
			}
		}

		result = append(result, discoveredField{key: key, fieldType: fieldType, desc: description})
	}
	return result
}

// inferFieldType maps a Go reflect.Type to a FieldType.
// Returns false for unsupported types (slices, maps, etc.).
func inferFieldType(t reflect.Type) (FieldType, bool) {
	// Unwrap pointer.
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return FieldBool, true
	case reflect.Int, reflect.Int64:
		return FieldInt, true
	case reflect.Float64:
		return FieldFloat, true
	case reflect.String:
		return FieldString, true
	}
	return 0, false
}

// ── Declarative registry ────────────────────────────────────────────

var configFields = buildRegistry(
	reg(AgentLoopConfig{}, global("agent_loop"), agent("agent_loop"),
		desc("max_output_tokens", "max tokens in model response"),
		desc("max_tool_loops", "max tool iterations per turn"),
		desc("duplicate_messages", "send user text twice per API call"),
		dur("cache_ttl", "prompt cache TTL: 5m or 1h"),
	),
	reg(NotifyConfig{}, global("notify"), agent("notify"), platform("notify"),
		desc("startup_notify", "send notification on startup"),
		desc("compaction_notify", "send notification on compaction"),
		desc("task_list_notify", "send notification on task list changes"),
	),
	reg(DebugConfig{}, global("debug"), agent("debug"), platform("debug"),
		desc("log_api_key_suffix", "log last 4 chars of API keys on provider calls"),
		desc("messages_in_log", "log user message content"),
		desc("inject_agent_warnings", "inject warnings into agent session: all, errors, off"),
		desc("inject_chat_warnings", "send warnings as chat notifications: all, errors, off"),
		desc("compaction_debug", "send compaction summary as file attachment"),
	),
	reg(DisplayConfig{}, global("display"), agent("display"), platform("display"),
		desc("show_tool_calls", "tool call display: off, preview, full"),
		desc("show_thinking", "thinking display: off, compact, true"),
		desc("stream_output", "stream model output"),
		dur("stream_interval", "interval between stream edits"),
		desc("display_width", "display width for dividers"),
	),
	reg(AccessConfig{}, platform("access")),
	reg(NudgeConfig{}, global("nudge"), agent("nudge"),
		desc("nudge_enable", "enable mid-turn behavioral reminders"),
		desc("nudge_auto_extract", "auto-extract rules from character files via LLM"),
		desc("nudge_cooldown", "min tool calls between repeating same reminder"),
		desc("nudge_max_per_batch", "max reminders injected per tool batch"),
		desc("nudge_pre_answer_gate", "enable pre-answer verification gate"),
		desc("nudge_pre_answer_min_tools", "min tool calls before pre-answer gate fires"),
		desc("nudge_default_enable", "enable built-in tool/skill reminders"),
		desc("nudge_default_frequency", "turns between tool/skill reminders (default 50)"),
		desc("nudge_default_scratchpad_frequency", "turns between scratchpad review reminders (0=disabled, default 20)"),
		desc("nudge_default_braindead_threshold", "consecutive tool loops before warning (0=disabled)"),
	),
	reg(VoiceConfig{}, global("voice"), agent("voice"),
		desc("tts", "TTS provider id"),
		desc("stt", "STT provider id"),
		desc("tts_rate", "TTS speech rate multiplier"),
	),
	reg(BehaviorConfig{}, global("behavior"), agent("behavior"),
		desc("steer_mode", "inject user messages between tool calls"),
		desc("enable_stop_aliases", "enable stop command aliases"),
		dur("group_throttle", "throttle between group model calls"),
		dur("turn_lock_warn_threshold", "warn if turn lock held longer than this"),
	),
	reg(SystemConfig{}, global("system"), agent("system")),
	reg(SessionsConfig{}, global("sessions"),
		desc("compaction_threshold", "compact at this fraction of context window"),
		desc("compaction_max_tokens", "max output tokens for summary"),
		desc("compaction_min_messages", "min messages before compacting"),
		desc("compaction_preserve_messages", "preserve last N messages through compaction"),
		desc("autocompact_before_mana_refresh_preserve_pct", "fraction of messages to preserve in mana-refresh mode"),
		desc("max_system_prompt_chars_file", "per-file char warning threshold"),
		desc("max_system_prompt_chars_total", "total system prompt char warning threshold"),
		desc("facet_no_compact", "set no_compact on facet sessions (default true)"),
		dur("archive_after", "archive idle sessions after this duration"),
	),
	reg(AgentSessionsOverride{}, agent("sessions")),
	reg(ToolsConfig{}, global("tools"),
		desc("max_result_chars", "max chars before writing result to file"),
		desc("auto_summarise", "auto-summarise oversized tool results"),
		desc("search_provider", "web search: brave or anthropic"),
		desc("fetch_provider", "web fetch: anthropic or builtin"),
		desc("todo_format", "todo list format: lines or table"),
		desc("exec_auto_background", "seconds before auto-backgrounding exec"),
		desc("exec_default_timeout", "default timeout for exec in seconds"),
		desc("tmux_autopilot", "auto-unwatch on inactivity"),
		dur("tmux_watch_threshold", "default watch threshold duration"),
		dur("tmux_session_ttl", "auto-kill idle tmux sessions after"),
		desc("tmux_cols", "tmux window columns"),
		desc("tmux_rows", "tmux window rows"),
		desc("max_concurrent_spawns", "max concurrent spawn sessions"),
		desc("explore_max_depth", "max tool loops for explore spawn"),
		dur("web_fetch_timeout", "HTTP timeout for web fetch"),
		dur("web_search_timeout", "HTTP timeout for web search"),
		desc("tool_call_preview_chars", "max chars for tool call preview"),
	),
	reg(AgentToolsOverride{}, agent("tools")),
	reg(KeepaliveConfig{}, global("keepalive"), agent("keepalive"),
		desc("enabled", "enable keepalive timer"),
		dur("interval", "time since cache last warmed"),
	),
	reg(BackgroundConfig{}, global("background"), agent("background"),
		desc("enabled", "enable background work timer"),
		dur("interval", "time since last interaction before firing"),
	),
	reg(MemoryFormationConfig{}, global("memory_formation"), agent("memory_formation"),
		dur("interval", "time between captures"),
		dur("consolidation_interval", "min time between consolidations"),
		desc("compaction_enabled", "memory capture before compaction"),
	),
	reg(ManaConfig{}, global("mana"), global("usage_warnings"),
		desc("name", "what to call quota (e.g. mana)"),
		desc("restore_threshold", "mana restore notice threshold (0=disabled)"),
		dur("invest_interval", "quiet period after mana reset"),
	),
	reg(MemoryConfig{}, global("memory"),
		dur("reindex_debounce", "delay before reindex"),
		desc("conversation_weight", "weight for conversation search results"),
		desc("search_limit", "max search results to return"),
		dur("sweep_interval", "periodic full reindex interval"),
	),
	reg(LoggingConfig{}, global("logging"),
		desc("level", "log level: DEBUG, INFO, WARN, ERROR"),
		desc("full_payload", "write full API payloads to file"),
		desc("cache_bust_detect", "alert on cache_read drop"),
		desc("log_rotation", "enable built-in log rotation"),
		dur("rotation_period", "how often to rotate logs"),
		dur("retention_period", "keep lines newer than this"),
	),
	reg(CacheConfig{}, global("cache"),
		desc("strategy", "cache strategy: auto or explicit"),
		desc("ttl", "Anthropic prompt cache TTL: 5m or 1h"),
	),
	reg(EnvironmentConfig{}, global("environment"),
		desc("enabled", "inject environment block"),
		desc("docs_path", "path to platform docs directory"),
	),
	reg(AnthropicConfig{}, global("anthropic"),
		dur("http_timeout", "HTTP timeout for API calls"),
		dur("usage_api_timeout", "usage API timeout"),
		dur("usage_cache_ttl", "usage data cache TTL"),
	),
	reg(GeminiConfig{}, global("gemini"),
		dur("http_timeout", "HTTP timeout for API calls"),
		dur("cache_ttl", "context cache TTL"),
	),
	reg(DatabaseConfig{}, global("database"),
		dur("busy_timeout", "SQLite busy timeout"),
	),
	reg(HTTPConfig{}, global("http"),
		desc("port", "HTTP server port"),
		desc("bind", "HTTP server bind address"),
	),
	reg(BrowserConfig{}, global("browser"), agent("browser"),
		desc("enabled", "enable browser tool"),
		desc("headless", "run headless"),
		desc("timeout_sec", "page operation timeout in seconds"),
		desc("incognito", "use incognito mode"),
	),
	reg(PlatformConfig{}, global("platforms"),
		dur("facet_session_ttl", "idle TTL before facet reclaim"),
		desc("message_queue_size", "message queue buffer size"),
	),
)
