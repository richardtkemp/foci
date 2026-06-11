package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ToolCallDisplay controls how tool calls are shown in Telegram.
type ToolCallDisplay string

const (
	ToolCallOff     ToolCallDisplay = "off"     // hidden
	ToolCallPreview ToolCallDisplay = "preview" // shown then overwritten by reply
	ToolCallFull    ToolCallDisplay = "full"    // shown and kept; reply is a separate message
)

// UnmarshalTOML accepts "off"/"false", "preview", or "full"/"true" (plus bool).
func (d *ToolCallDisplay) UnmarshalTOML(v any) error {
	var val string
	switch tv := v.(type) {
	case string:
		val = strings.ToLower(tv)
	case bool:
		if tv {
			*d = ToolCallFull
		} else {
			*d = ToolCallOff
		}
		return nil
	default:
		return fmt.Errorf("show_tool_calls must be a string or bool")
	}
	switch val {
	case "off", "false":
		*d = ToolCallOff
		return nil
	case "preview", "medium":
		*d = ToolCallPreview
		return nil
	case "full", "true":
		*d = ToolCallFull
		return nil
	default:
		return fmt.Errorf("invalid show_tool_calls value %q (must be off/false, preview/medium, full/true)", val)
	}
}

// ShowThinking controls how thinking blocks are displayed in Telegram.
type ShowThinking string

const (
	ShowThinkingOff     ShowThinking = "off"     // thinking stripped, not shown
	ShowThinkingCompact ShowThinking = "compact" // response with "Show thinking" toggle button
	ShowThinkingTrue    ShowThinking = "true"    // thinking prepended to every response
)

// UnmarshalTOML accepts "off"/"false", "compact", or "true"/"full" (plus bool).
func (s *ShowThinking) UnmarshalTOML(v any) error {
	var val string
	switch tv := v.(type) {
	case string:
		val = strings.ToLower(tv)
	case bool:
		if tv {
			*s = ShowThinkingTrue
		} else {
			*s = ShowThinkingOff
		}
		return nil
	default:
		return fmt.Errorf("show_thinking must be a string or bool")
	}
	switch val {
	case "off", "false":
		*s = ShowThinkingOff
		return nil
	case "compact", "medium":
		*s = ShowThinkingCompact
		return nil
	case "true", "full":
		*s = ShowThinkingTrue
		return nil
	default:
		return fmt.Errorf("invalid show_thinking value %q (must be off/false, compact/medium, true/full)", val)
	}
}

// InjectionLevel controls whether and what severity of log warnings are injected.
type InjectionLevel string

const (
	InjectionAll  InjectionLevel = "all"  // inject WARN + ERROR
	InjectionErrors InjectionLevel = "errors" // inject ERROR only
	InjectionOff  InjectionLevel = "off"  // disabled
)

// UnmarshalTOML accepts "off"/"false", "errors", or "all"/"true"/"full" (plus bool).
func (il *InjectionLevel) UnmarshalTOML(v any) error {
	var val string
	switch tv := v.(type) {
	case string:
		val = strings.ToLower(tv)
	case bool:
		if tv {
			*il = InjectionAll
		} else {
			*il = InjectionOff
		}
		return nil
	default:
		return fmt.Errorf("injection level must be a string or bool")
	}
	switch val {
	case "off", "false":
		*il = InjectionOff
		return nil
	case "errors", "medium":
		*il = InjectionErrors
		return nil
	case "all", "true", "full":
		*il = InjectionAll
		return nil
	case "":
		*il = ""
		return nil
	default:
		return fmt.Errorf("invalid injection level %q (must be off/false, errors/medium, all/true/full)", val)
	}
}

// Enabled returns true if injection is active (all or errors).
func (il InjectionLevel) Enabled() bool {
	return il == InjectionAll || il == InjectionErrors
}

// IncludeWarnings returns true if WARN-level entries should be included.
func (il InjectionLevel) IncludeWarnings() bool {
	return il == InjectionAll
}

// ManaConfig holds mana/quota budget and warning settings.
// Embed in Config (global, TOML [mana]) and AgentConfig (per-agent).
// All fields are pointer/slice types for Merge-based resolution.
type ManaConfig struct {
	Name             *string `toml:"name"              default:"mana"  desc:"what to call quota (e.g. mana)"` // what to call quota
	Thresholds       []int   `toml:"thresholds"`                       // mana percentages to warn at (e.g. [50, 25, 10, 5])
	RestoreThreshold *int    `toml:"restore_threshold" desc:"mana restore notice threshold (0=disabled)" min:"0" max:"100"` // inject session notice when mana restores to 100% after being below this (0=disabled)
	InvestInterval   *string `toml:"invest_interval"   default:"30m"  desc:"quiet period after mana reset" type:"duration"` // quiet period after mana reset before spending
}


// ContextWindow represents a model's context window size in tokens.
// Accepts plain integers or strings with k/K suffix (1k = 1000 tokens).
type ContextWindow int

// UnmarshalTOML accepts both int (context = 131072) and string (context = "262k").
func (c *ContextWindow) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case int64:
		*c = ContextWindow(val)
		return nil
	case string:
		return c.parse(val)
	default:
		return fmt.Errorf("context: expected int or string, got %T", v)
	}
}

func (c *ContextWindow) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*c = 0
		return nil
	}
	multiplier := 1
	if strings.HasSuffix(s, "k") || strings.HasSuffix(s, "K") {
		multiplier = 1000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "M") {
		multiplier = 1000000
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("context: invalid value %q", s)
	}
	*c = ContextWindow(n * multiplier)
	return nil
}

// ThinkingMode controls whether model thinking/reasoning is enabled.
type ThinkingMode string

// UnmarshalTOML accepts "off"/"false" or "adaptive"/"true" (plus bool).
func (t *ThinkingMode) UnmarshalTOML(v any) error {
	var val string
	switch tv := v.(type) {
	case string:
		val = strings.ToLower(tv)
	case bool:
		if tv {
			*t = "adaptive"
		} else {
			*t = "off"
		}
		return nil
	default:
		return fmt.Errorf("thinking must be a string or bool")
	}
	switch val {
	case "off", "false":
		*t = "off"
		return nil
	case "adaptive", "true":
		*t = "adaptive"
		return nil
	case "":
		*t = ""
		return nil
	default:
		return fmt.Errorf("invalid thinking value %q (must be off/false, adaptive/true)", val)
	}
}

// ModelConfig defines a named model with its settings.
// Used in [models.*] TOML sections.
type ModelConfig struct {
	Model           string        `toml:"model"`            // "developer/model_id" (required)
	Endpoint        string        `toml:"endpoint"`         // explicit endpoint override (optional; empty = auto-select from developer)
	Thinking        ThinkingMode  `toml:"thinking"`         // "adaptive", "off", or bool via UnmarshalTOML
	Effort          string        `toml:"effort"`           // "low", "medium", "high"
	Speed           string        `toml:"speed"`            // "fast" or ""
	Context         ContextWindow `toml:"context"`          // context window size in tokens (e.g. 262000 or "262k")
	EnableKeepalive *bool         `toml:"enable_keepalive"`  // nil=auto-detect, true/false=explicit
	CacheTTL        string        `toml:"cache_ttl"`         // cache TTL: Go duration, empty=auto-detect (Anthropic: "5m"/"1h"; Gemini: any duration)
	CacheStrategy   string        `toml:"cache_strategy"`    // cache marker strategy: "auto" or "explicit" (Anthropic only, default "auto")
}

// CCBackendConfig holds defaults shared by all Claude Code-based delegator
// backends (cctmux, ccstream). Per-agent [agents.backend_config] values
// still apply; scalar values there override, and DefaultAllowedTools is
// merged with any per-agent allowed_tools rather than replaced.
type CCBackendConfig struct {
	// DefaultAllowedTools is a list of Claude Code permission rules (same
	// syntax as settings.json permissions.allow — e.g. "Write(/tmp/**)",
	// "Bash(git:*)") that every CC-backed agent receives via --allowedTools.
	// Merged with per-agent backend_config.allowed_tools before launch.
	// Factory default grants /tmp file writes so agents don't prompt for
	// scratch-file access. Set to an empty list in TOML to disable.
	DefaultAllowedTools []string `toml:"default_allowed_tools"`

	// ClaudeBinary overrides the path/name of the `claude` executable
	// foci spawns for CC-backed agents. Default "" → falls back to "claude"
	// (resolved via $PATH). Used by integration tests to point at a stub
	// binary (bin/cc-stub) that mimics the CC stream-json protocol.
	// Per-agent backend_config.claude_binary takes precedence if set.
	ClaudeBinary string `toml:"claude_binary"`
}

// GroupsConfig assigns named models to groups and call sites.
// Groups is populated from top-level string keys in [groups] by load.go
// (not decoded by TOML directly since the section mixes string keys with sub-tables).
// Users can define arbitrary groups; "powerful" is required, "fast"/"cheap" default to it.
type GroupsConfig struct {
	Groups    map[string]string `toml:"-"`          // group name → model (populated by load.go from TOML metadata)
	Calls     map[string]string `toml:"calls"`      // call site → group overrides
	Fallbacks map[string]string `toml:"fallbacks"`  // model → fallback model
}

type AgentConfig struct {
	ID        string `toml:"id"`
	Name      string `toml:"name"`      // human-readable name (e.g. "Clutch"); used in voice endpoint agent list
	Emoji     string `toml:"emoji"`     // emoji for agent (e.g. "🥔"); used in voice endpoint agent list
	Workspace string `toml:"workspace"`

	Memory    MemoryConfig      `toml:"memory"`    // per-agent memory overrides (sources combined with global [memory])
	Platforms []PlatformConfig  `toml:"platforms"` // per-agent platform configurations

	// Per-agent overrides — resolved via Merge at use time
	// (e.g. config.Merge(acfg.Nudge, cfg.Defaults.Nudge)).
	Notify   NotifyConfig    `toml:"notify"`   // overrides from [defaults.notify]
	Display  DisplayConfig   `toml:"display"`  // overrides from [defaults.display]
	Nudge    NudgeConfig     `toml:"nudge"`    // overrides from [defaults.nudge]
	Voice    VoiceConfig     `toml:"voice"`    // overrides from [defaults.voice]
	Loop     AgentLoopConfig `toml:"loop"`     // overrides from [defaults.loop]
	Behavior BehaviorConfig  `toml:"behavior"` // overrides from [defaults.behavior]
	System   SystemConfig    `toml:"system"`   // overrides from [defaults.system]

	Sessions        AgentSessionsOverride `toml:"sessions"`          // overrides from [sessions]
	Tools           AgentToolsOverride    `toml:"tools"`             // overrides from [tools]
	Debug           DebugConfig           `toml:"debug"`             // overrides from [debug]
	Environment     EnvironmentConfig     `toml:"environment"`       // overrides from [environment]
	Browser         BrowserConfig         `toml:"browser"`           // overrides from [browser]
	Keepalive       KeepaliveConfig       `toml:"keepalive"`         // overrides from [keepalive]
	Background      BackgroundConfig      `toml:"background"`        // overrides from [background]
	Reflection ReflectionConfig `toml:"reflection"`  // overrides from [reflection]
	Mana            ManaConfig            `toml:"mana"`              // overrides from [mana]
	Groups          GroupsConfig          `toml:"groups"`            // overrides from [groups]
	Permissions     PermissionsConfig     `toml:"permissions"`       // overrides from [permissions]

	// Backend selection: empty or "api" = traditional agent loop.
	// A coding agent name (e.g. "claude-code-tmux", "codex", "opencode") delegates
	// entire turns to an external agent subprocess.
	Backend       string         `toml:"backend"`
	BackendConfig map[string]any `toml:"backend_config"` // backend-specific settings

	// Per-agent skills and message transforms (empty = use global)
	SkillsDir         string             `toml:"skills_dir"`         // per-agent skills directory (default: $workspace/skills/)
	MessageTransforms []MessageTransform `toml:"message_transforms"` // regex find/replace rules (empty = use global)
	BlockedPaths      []BlockedPath      `toml:"blocked_paths"`      // path prefixes that write/edit tools refuse (empty = use global)
}

// AgentSessionsOverride groups the config from [sessions] that can be overridden per-agent.
type AgentSessionsOverride struct {
	CompactionConfig
	BranchOrientationFacetPrompt    *string `toml:"branch_orientation_facet_prompt"`
	BranchOrientationHeadlessPrompt *string `toml:"branch_orientation_headless_prompt"`
}

// AgentToolsOverride groups the config groups whose global home is [tools].
type AgentToolsOverride struct {
	ToolConfig
	SummaryConfig
}

// CompactionConfig holds compaction settings. Embed in SessionsConfig (global) and AgentConfig (per-agent).
type CompactionConfig struct {
	CompactionThreshold                     *float64 `toml:"compaction_threshold"  default:"0.8"  desc:"compact at this fraction of context window" min:"0" max:"1"`
	CompactionSummaryPrompt                 *string  `toml:"compaction_summary_prompt"             desc:"compaction summary prompt file path"`
	CompactionHandoffMsg                    *string  `toml:"compaction_handoff_msg"                desc:"compaction handoff message file path"`
	CompactionPreserveMessages              *int     `toml:"compaction_preserve_messages"          desc:"preserve last N messages through compaction" min:"0"`
	CompactionEffort                        *string  `toml:"compaction_effort"                     desc:"compaction effort level"`
	FacetNoCompact                          *bool    `toml:"facet_no_compact"                      desc:"set no_compact on facet sessions (default true)"`
	AutocompactBeforeManaRefresh            *bool    `toml:"autocompact_before_mana_refresh"       desc:"autocompact before mana refresh"`
	AutocompactBeforeManaRefreshThreshold   *string  `toml:"autocompact_before_mana_refresh_threshold" desc:"mana threshold to trigger autocompact" type:"duration"`
	AutocompactBeforeManaRefreshFactor      *float64 `toml:"autocompact_before_mana_refresh_factor" desc:"compaction threshold factor for mana refresh"`
	AutocompactBeforeManaRefreshPreserve    *int     `toml:"autocompact_before_mana_refresh_preserve" desc:"messages to preserve in mana-refresh compaction"`
	AutocompactBeforeManaRefreshPreservePct *float64 `toml:"autocompact_before_mana_refresh_preserve_pct" desc:"fraction of messages to preserve in mana-refresh mode (0.0-1.0)" min:"0" max:"1"`
}

// NudgeConfig holds nudge system settings.
// Global: [nudge], per-agent: [[agents]].nudge.*
type NudgeConfig struct {
	NudgeEnable                     *bool   `toml:"nudge_enable"                      default:"true"  desc:"enable mid-turn behavioral reminders"`
	NudgeAutoExtract                *bool   `toml:"nudge_auto_extract"                default:"true"  desc:"auto-extract rules from character files via LLM"`
	NudgeCooldown                   *int    `toml:"nudge_cooldown"                                    desc:"min tool calls between repeating same reminder"`
	NudgeMaxPerBatch                *int    `toml:"nudge_max_per_batch"                               desc:"max reminders injected per tool batch"`
	NudgePreAnswerGate              *bool   `toml:"nudge_pre_answer_gate"                             desc:"enable pre-answer verification gate"`
	NudgePreAnswerMinTools          *int    `toml:"nudge_pre_answer_min_tools"          default:"2"   desc:"min tool calls before pre-answer gate fires"`
	NudgeDefaultEnable              *bool   `toml:"nudge_default_enable"              default:"true"  desc:"enable built-in tool/skill reminders"`
	NudgeDefaultFrequency           *int    `toml:"nudge_default_frequency"             default:"50"  desc:"turns between tool/skill reminders (default 50)"`
	NudgeDefaultScratchpadFrequency *int    `toml:"nudge_default_scratchpad_frequency"                desc:"turns between scratchpad review reminders (0=disabled, default 20)"`
	NudgeDefaultBraindeadThreshold  *int    `toml:"nudge_default_braindead_threshold"                 desc:"consecutive tool loops before warning (0=disabled)"`
	NudgeDefaultBraindeadPrompt     *string `toml:"nudge_default_braindead_prompt"`
}

// SummaryConfig holds tool result summarisation settings.
// Embed in ToolsConfig (global) and AgentToolsOverride (per-agent).
type SummaryConfig struct {
	MaxResultChars       *int  `toml:"max_result_chars"                       desc:"max chars before writing result to file"`
	MaxSummaryChars      *int  `toml:"max_summary_chars"                      desc:"max chars in summary output"`
	AutoSummarise        *bool `toml:"auto_summarise"         default:"true"  desc:"auto-summarise oversized tool results"`
	SummaryContextTurns  *int  `toml:"summary_context_turns"                  desc:"context turns to include in summary"`
	SummaryContextChars  *int  `toml:"summary_context_chars"                  desc:"max context chars for summary"`
	MaxSummaryInputChars *int  `toml:"max_summary_input_chars"                desc:"max input chars for summary generation"`
	MaxImagePixels       *int  `toml:"max_image_pixels"                       desc:"max image pixels before downscaling"`
}

// Voice WebSocket resource limits (P1-10). Defaults live in the VoiceConfig
// `default:` tags; these consts are the single source the runtime falls back to
// when a config value is absent (e.g. a config built without ApplyTagDefaults).
const (
	DefaultVoiceMaxFrameBytes = 1 << 20  // 1 MiB — max single inbound WS frame
	DefaultVoiceMaxAudioBytes = 50 << 20 // 50 MiB — max accumulated audio buffer

	// DefaultVoiceMaxConcurrentTurns bounds in-flight STT→agent→TTS goroutines
	// per WebSocket connection so a client can't spawn unbounded goroutines by
	// flooding audio_end/text frames. Turns serialise on turnMu anyway; this
	// just caps how many may queue.
	DefaultVoiceMaxConcurrentTurns = 4

	// DefaultVoiceHTTPMaxResponseBytes caps an STT/TTS HTTP response body
	// (TTS audio is the larger side). Guards io.ReadAll against an
	// unbounded/malicious upstream.
	DefaultVoiceHTTPMaxResponseBytes = 64 << 20 // 64 MiB
)

// DefaultVoiceHTTPTimeout is the fallback timeout for STT/TTS HTTP calls when
// [voice] http_timeout is unset (mirrors the VoiceConfig default tag).
const DefaultVoiceHTTPTimeout = "60s"

// VoiceConfig holds TTS/STT settings.
// Global: [voice], per-agent: [[agents]].voice.*
type VoiceConfig struct {
	TTS             *string           `toml:"tts"      desc:"TTS provider id"`
	STT             *string           `toml:"stt"      desc:"STT provider id"`
	TTSRate         *float64          `toml:"tts_rate"  desc:"TTS speech rate multiplier"`
	TTSReplacements map[string]string `toml:"tts_replacements"`
	STTReplacements map[string]string `toml:"stt_replacements"`
	MaxFrameBytes   *int              `toml:"max_frame_bytes" default:"1048576"  desc:"max single inbound websocket frame in bytes"`
	MaxAudioBytes   *int              `toml:"max_audio_bytes" default:"52428800" desc:"max accumulated voice audio buffer in bytes"`
	MaxConcurrentTurns   *int         `toml:"max_concurrent_turns" default:"4" desc:"max in-flight STT/agent/TTS turns per voice connection"`
	HTTPTimeout          *string      `toml:"http_timeout" default:"60s" desc:"timeout for STT/TTS HTTP calls"`
	HTTPMaxResponseBytes *int         `toml:"http_max_response_bytes" default:"67108864" desc:"max STT/TTS HTTP response size in bytes"`
}

// AgentLoopConfig holds settings consumed by agent.HandleTurn().
// Global: [agent_loop], per-agent: [[agents]].agent_loop.*
type AgentLoopConfig struct {
	MaxOutputTokens               *int    `toml:"max_output_tokens"                desc:"max tokens in model response"`
	MaxToolLoops                  *int    `toml:"max_tool_loops"                   desc:"max tool iterations per turn"`
	DuplicateMessages             *bool   `toml:"duplicate_messages"               desc:"send user text twice per API call"`
	BatchPartialAssistantMessages *bool   `toml:"batch_partial_assistant_messages"  desc:"batch partial assistant messages"`
	BatchPartialJoiner            *string `toml:"batch_partial_joiner"             desc:"joiner string for batched partial messages"`
}

// BehaviorConfig holds agent behavioral settings.
// Global: [behavior], per-agent: [[agents]].behavior.*
type BehaviorConfig struct {
	SteerMode             *bool    `toml:"steer_mode"               default:"true"  desc:"inject user messages between tool calls"`
	GroupThrottle         *string  `toml:"group_throttle"                            desc:"group chat throttle duration" type:"duration"`
	TurnLockWarnThreshold *string  `toml:"turn_lock_warn_threshold" default:"3m"    desc:"warn when turn lock held longer than this" type:"duration"`
	EnableStopAliases     *bool    `toml:"enable_stop_aliases"      default:"true"  desc:"enable stop command aliases"`
	StopAliases           []string `toml:"stop_aliases"`
}

// SystemConfig holds system-level agent settings.
// Global: [system], per-agent: [[agents]].system.*
type SystemConfig struct {
	SystemFiles []string          `toml:"system_files"`
	Webhooks    map[string]string `toml:"webhooks"`
}

// ToolConfig holds per-agent tool behavioral overrides.
// Embed in ToolsConfig (global home) and AgentConfig (per-agent).
type ToolConfig struct {
	ExecAutoBackground  *int    `toml:"exec_auto_background"  default:"10"        desc:"seconds before auto-backgrounding exec"`
	MaxConcurrentSpawns *int    `toml:"max_concurrent_spawns" default:"3"         desc:"max concurrent spawn sessions"`
	ExploreMaxDepth     *int    `toml:"explore_max_depth"     default:"100"       desc:"max tool loops for explore spawn"`
	MaxUploadFileSize   *int64  `toml:"max_upload_file_size"  default:"52428800"  desc:"max upload file size in bytes"` // 50MB
	TmuxAutopilot       *bool   `toml:"tmux_autopilot"        default:"true"      desc:"auto-unwatch on inactivity"`
	TmuxWatchThreshold  *string `toml:"tmux_watch_threshold"  default:"30s"       desc:"default watch threshold duration" type:"duration"`
	TmuxSessionTTL      *string `toml:"tmux_session_ttl"      default:"24h"       desc:"auto-kill idle tmux sessions after" type:"duration"`
	SearchProvider      *string `toml:"search_provider"       default:"brave"     desc:"web search: brave or anthropic" choices:"brave,anthropic"`
	FetchProvider       *string `toml:"fetch_provider"        default:"builtin"   desc:"web fetch: anthropic or builtin" choices:"anthropic,builtin"`
	TodoFormat          *string `toml:"todo_format"                                desc:"todo list format: lines or table" choices:"lines,table"`
}

type AnthropicConfig struct {
	UsageAPITimeout   string `toml:"usage_api_timeout"   default:"10s" desc:"HTTP timeout for usage API calls" type:"duration"`
	UsageCacheTTL     string `toml:"usage_cache_ttl"     default:"10m" desc:"cache TTL for usage API responses" type:"duration"`
	CCExpiryThreshold string `toml:"cc_expiry_threshold" default:"5m"  desc:"proactive token refresh threshold" type:"duration"`
}

// DisplayConfig holds display-related settings that can be set at any level
// of the configuration cascade. All fields are pointer types so Merge can
// distinguish "not set" from "set to zero value".
type DisplayConfig struct {
	ShowToolCalls         *ToolCallDisplay `toml:"show_tool_calls"         desc:"tool call display: off, preview, full" choices:"off,preview,full"` // tool call display: off, preview, full
	ShowThinking          *ShowThinking    `toml:"show_thinking"           desc:"thinking display: off, compact, true" choices:"off,compact,true"`  // thinking display: off, compact, true
	StreamOutput          *bool            `toml:"stream_output"           desc:"stream model output"`          // stream model output in real-time
	StreamInterval        *string          `toml:"stream_interval"         desc:"interval between stream edits" type:"duration"` // duration between message edits during streaming
	Streaming             *bool            `toml:"streaming"               desc:"use streaming API"`              // use streaming API
	DisplayWidth          *int             `toml:"display_width"           desc:"display width for dividers"`          // display width for dividers
	ReceivedFilesDir      *string          `toml:"received_files_dir"      desc:"save received files to this directory"`     // save received files to this directory
	InjectedMessageHeader *string          `toml:"injected_message_header" desc:"header prepended to injected messages"` // header prepended to injected messages
}

// AccessConfig holds access control settings that can be set at any level
// of the configuration cascade.
type AccessConfig struct {
	AllowedUsersOnly *bool    `toml:"allowed_users_only" default:"true" desc:"require allowed_users; when false, accept messages from any user"`
	AllowedUsers     []string `toml:"allowed_users"`                                                                                              // platform-specific user IDs allowed to interact
	RequireMention   *bool    `toml:"require_mention"`                                                                                            // require @mention in group chats
}

// NotifyConfig holds notification/warning settings that can be configured at
// any scope level. Resolution follows the 5-level cascade via Merge.
// All fields are nillable so nil means "not set, inherit from wider scope."
type NotifyConfig struct {
	InjectAgentWarnings *InjectionLevel `toml:"inject_agent_warnings"                   desc:"inject warnings into agent session: all, errors, off"` // inject warnings/errors into agent session
	InjectChatWarnings  *InjectionLevel `toml:"inject_chat_warnings"                    desc:"send warnings as chat notifications: all, errors, off"` // send warnings/errors as chat notifications
	StartupNotify       *bool           `toml:"startup_notify"           default:"true" desc:"send notification on startup"` // send startup notification
	CompactionNotify    *bool           `toml:"compaction_notify"        default:"true" desc:"send notification on compaction"` // send notification on compaction
	TaskListNotify      *bool           `toml:"task_list_notify"         default:"true" desc:"send notification on task list changes"` // send notification on task list changes
	CompactionDebug     *bool           `toml:"compaction_debug"                        desc:"send compaction summary as file attachment"` // send compaction summary as file attachment
	WarningMaxPerWindow *int            `toml:"warning_max_per_window"   default:"3"    desc:"max identical warnings per window before suppression"` // max identical warnings per window before suppression (default 3)
}

// InjectAgentWarningsLevel returns the resolved injection level (default: off).
func (n NotifyConfig) InjectAgentWarningsLevel() InjectionLevel {
	if n.InjectAgentWarnings != nil {
		return *n.InjectAgentWarnings
	}
	return InjectionOff
}

// InjectChatWarningsLevel returns the resolved injection level (default: off).
func (n NotifyConfig) InjectChatWarningsLevel() InjectionLevel {
	if n.InjectChatWarnings != nil {
		return *n.InjectChatWarnings
	}
	return InjectionOff
}

// StartupNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) StartupNotifyEnabled() bool {
	if n.StartupNotify != nil {
		return *n.StartupNotify
	}
	return true
}

// CompactionNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) CompactionNotifyEnabled() bool {
	if n.CompactionNotify != nil {
		return *n.CompactionNotify
	}
	return true
}

// TaskListNotifyEnabled returns the resolved value (default: true).
func (n NotifyConfig) TaskListNotifyEnabled() bool {
	if n.TaskListNotify != nil {
		return *n.TaskListNotify
	}
	return true
}

// CompactionDebugEnabled returns the resolved value (default: false).
func (n NotifyConfig) CompactionDebugEnabled() bool {
	if n.CompactionDebug != nil {
		return *n.CompactionDebug
	}
	return false
}

// PlatformConfig is the unified platform configuration used for both global
// [[platforms]] entries and per-agent [[agents.platforms]] overrides.
type PlatformConfig struct {
	ID string `toml:"id"`

	// Config groups (cascade via Merge)
	Notify  NotifyConfig  `toml:"notify"`
	Debug   DebugConfig   `toml:"debug"`
	Display DisplayConfig `toml:"display"`
	Access  AccessConfig  `toml:"access"`

	// Shared platform fields
	Bot              string   `toml:"bot"`
	BotSecret        string   `toml:"bot_secret"`
	FacetBots        []string `toml:"facet_bots"`
	FacetSessionTTL  string   `toml:"facet_session_ttl"  desc:"idle TTL before facet reclaim" type:"duration"`
	MessageQueueSize int      `toml:"message_queue_size" desc:"message queue buffer size"`

	// Platform-specific subsections (at most one non-nil, must match ID)
	Telegram *TelegramSpecific `toml:"telegram"`
	Discord  *DiscordSpecific  `toml:"discord"`
}

// Platform returns the PlatformConfig with the given ID, or nil.
func (c *Config) Platform(id string) *PlatformConfig {
	for i := range c.Platforms {
		if c.Platforms[i].ID == id {
			return &c.Platforms[i]
		}
	}
	return nil
}

// Platform returns the PlatformConfig with the given ID from agent platforms, or nil.
func (a *AgentConfig) Platform(id string) *PlatformConfig {
	for i := range a.Platforms {
		if a.Platforms[i].ID == id {
			return &a.Platforms[i]
		}
	}
	return nil
}

// SafeNotify returns the NotifyConfig from a *PlatformConfig, or zero if nil.
func (p *PlatformConfig) SafeNotify() NotifyConfig {
	if p == nil {
		return NotifyConfig{}
	}
	return p.Notify
}

// SafeDebug returns the DebugConfig from a *PlatformConfig, or zero if nil.
func (p *PlatformConfig) SafeDebug() DebugConfig {
	if p == nil {
		return DebugConfig{}
	}
	return p.Debug
}

// SafeDisplay returns the DisplayConfig from a *PlatformConfig, or zero if nil.
func (p *PlatformConfig) SafeDisplay() DisplayConfig {
	if p == nil {
		return DisplayConfig{}
	}
	return p.Display
}

// TelegramSpecific holds Telegram-only config fields.
type TelegramSpecific struct {
	LongPollTimeout string  `toml:"long_poll_timeout"` // default "30s" (HTTP-client timeout; Telegram-side long-poll derived as -5s)
	TableWrapLines  *int    `toml:"table_wrap_lines"  desc:"max wrapped lines per table cell"` // default 5
	TableStyle      *string `toml:"table_style"       desc:"table style: pretty or markdown" choices:"pretty,markdown"` // default "pretty"

	// APIBase overrides the Telegram Bot API base URL (default
	// "https://api.telegram.org"). Used by integration tests to point bots
	// at a local httptest stub. Empty = library default. The trailing
	// slash, if present, is trimmed by gotgbot.
	APIBase string `toml:"api_base"`
}

// DiscordSpecific holds Discord-only config fields.
type DiscordSpecific struct {
	AutoThread *bool  `toml:"auto_thread"` // default true
	GuildID    string `toml:"guild_id"`
}

// ApplyDefaults fills zero-value fields from the given defaults.
func (p *PlatformConfig) ApplyDefaults(defaults PlatformConfig) {
	p.Notify = Merge(p.Notify, defaults.Notify)
	p.Debug = Merge(p.Debug, defaults.Debug)
	p.Display = Merge(p.Display, defaults.Display)
	p.Access = Merge(p.Access, defaults.Access)
	if p.FacetSessionTTL == "" {
		p.FacetSessionTTL = defaults.FacetSessionTTL
	}
	if p.MessageQueueSize == 0 {
		p.MessageQueueSize = defaults.MessageQueueSize
	}
	// Platform-specific: agent inherits the entire sub-block if not set.
	if p.Telegram == nil && defaults.Telegram != nil {
		copy := *defaults.Telegram
		p.Telegram = &copy
	} else if p.Telegram != nil && defaults.Telegram != nil {
		if p.Telegram.LongPollTimeout == "" {
			p.Telegram.LongPollTimeout = defaults.Telegram.LongPollTimeout
		}
		if p.Telegram.TableWrapLines == nil {
			p.Telegram.TableWrapLines = defaults.Telegram.TableWrapLines
		}
		if p.Telegram.TableStyle == nil {
			p.Telegram.TableStyle = defaults.Telegram.TableStyle
		}
		if p.Telegram.APIBase == "" {
			p.Telegram.APIBase = defaults.Telegram.APIBase
		}
	}
	if p.Discord != nil && defaults.Discord != nil {
		if p.Discord.AutoThread == nil {
			p.Discord.AutoThread = defaults.Discord.AutoThread
		}
		if p.Discord.GuildID == "" {
			p.Discord.GuildID = defaults.Discord.GuildID
		}
	}
}

type SessionsConfig struct {
	Dir string `toml:"dir"`

	CompactionConfig  // compaction settings (global defaults, overridable per-agent)
	CompactionMaxTokens   int `toml:"compaction_max_tokens"         default:"4096"  desc:"max output tokens for summary" min:"0"` // max output tokens for summary
	CompactionMinMessages int `toml:"compaction_min_messages"       default:"4"     desc:"min messages before compacting" min:"0"` // min messages before compacting
	MaxSystemPromptFile   int `toml:"max_system_prompt_chars_file"  default:"20000" desc:"per-file char warning threshold"` // per-file char threshold for warnings
	MaxSystemPromptTotal  int `toml:"max_system_prompt_chars_total" default:"80000" desc:"total system prompt char warning threshold"` // total system prompt char threshold

	BranchOrientationFacetPrompt    *string `toml:"branch_orientation_facet_prompt"`    // path to prompt file for user-attached facet branches
	BranchOrientationHeadlessPrompt *string `toml:"branch_orientation_headless_prompt"` // path to prompt file for headless branches (cron, spawn, keepalive)

	ArchiveAfter string `toml:"archive_after" default:"24h"`  // gzip idle sessions after this duration (default "24h")
	FileMode     string `toml:"file_mode"     default:"0600"` // octal file permissions for session files (default "0600")
}

type MemorySource struct {
	Name   string  `toml:"name"`   // unique identifier (e.g., "canonical", "code", "docs")
	Dir    string  `toml:"dir"`    // directory path to index
	Weight float64 `toml:"weight"` // weight multiplier: 0.0-1.0 (1.0 = highest priority)
}

// MemoryConfig holds memory system settings. Used both at global ([memory])
// and per-agent ([[agents]].memory) scope. All non-source fields are pointer
// types for Merge-based resolution (per-agent → global).
// Sources are combined additively (not merged) — see load.go.
type MemoryConfig struct {
	Sources            []MemorySource `toml:"sources"`
	SearchBackend      *string        `toml:"search_backend"      default:"bleve" desc:"search backend: fts5 or bleve"` // search backend: "fts5" or "bleve"
	ReindexDebounce    *string        `toml:"reindex_debounce"    desc:"delay before reindex" type:"duration"` // delay before reindex (e.g., "500ms", "2s"), default "0s"
	ConversationWeight *float64       `toml:"conversation_weight" default:"0.1"   desc:"weight for conversation search results" min:"0" max:"1"` // weight multiplier for conversation search results (default 0.1)
	SearchLimit        *int           `toml:"search_limit"        default:"20"    desc:"max search results to return"` // max search results to return (default 20)
	SweepInterval      *string        `toml:"sweep_interval"      default:"1h"    desc:"periodic full reindex interval" type:"duration"` // periodic full reindex interval (default "1h", "0" disables)
}


type DatabaseConfig struct {
	BusyTimeout string `toml:"busy_timeout" default:"5s" desc:"SQLite busy timeout" type:"duration"` // SQLite busy timeout for concurrent access (default "5s")
}

type HTTPConfig struct {
	Port                    int    `toml:"port" default:"18791" desc:"HTTP server port" min:"1" max:"65535"`
	Bind                    string `toml:"bind" default:"127.0.0.1" desc:"HTTP server bind address"`
	GracefulShutdownTimeout string `toml:"graceful_shutdown_timeout" default:"30s"` // time to wait for in-flight requests on shutdown (default "30s")
	WSEnabled               bool   `toml:"ws_enabled"`                // enable /voice WebSocket endpoint (default false)
	SocketPath              string `toml:"socket_path"`               // Unix socket path for same-user auth (default: auto-resolved to data dir)
}

type LoggingConfig struct {
	Level            string `toml:"level"      default:"INFO" desc:"log level: DEBUG, INFO, WARN, ERROR" choices:"DEBUG,INFO,WARN,ERROR"`
	EventFile        string `toml:"event_file" default:"logs/foci.log"`
	APIFile          string `toml:"api_file"   default:"logs/api.jsonl"`
	APIDB            string `toml:"api_db" default:"api.db"` // SQLite API call log path (relative to data_dir)
	ConversationLog *bool `toml:"conversation_log" default:"true" desc:"enable per-agent conversation logging"`

	FullPayload          bool   `toml:"full_payload"                         desc:"write full API payloads to file"` // write full API payloads to api-payload.jsonl
	PayloadFile          string `toml:"payload_file"             default:"logs/api-payload.jsonl"` // path for full API payload log

	WarningWindowDuration             string `toml:"warning_window_duration"              default:"5m"  desc:"time window for warning dedup" type:"duration"` // time window for warning dedup (default "5m")
	WarningProactiveActiveInterval    string `toml:"warning_proactive_active_interval"    default:"5m"  desc:"min interval between proactive warnings (active user)" type:"duration"` // min interval between proactive warning turns when user is active (default "5m")
	WarningProactiveInactiveInterval  string `toml:"warning_proactive_inactive_interval"  default:"1h"  desc:"min interval between proactive warnings (inactive user)" type:"duration"` // min interval when user is inactive (default "1h")
	WarningProactiveActivityThreshold string `toml:"warning_proactive_activity_threshold" default:"10m" desc:"user is active if last message within this window" type:"duration"` // user is "active" if last message within this window (default "10m")

	LogRotation         *bool  `toml:"log_rotation"            default:"true" desc:"enable built-in log rotation"` // enable built-in log rotation (default true)
	RotationPeriod      string `toml:"rotation_period"        default:"24h"   desc:"how often to rotate logs" type:"duration"` // how often to rotate (default "24h")
	RetentionPeriod     string `toml:"retention_period"       default:"48h"   desc:"keep lines newer than this" type:"duration"` // keep lines newer than this (default "48h")
	RotationMaxLineSize string `toml:"rotation_max_line_size" default:"64MB"  desc:"max line size for scanner buffer"` // max line size for scanner buffer (default "64MB")
	ArchiveDir          string `toml:"archive_dir"`            // gzip archive directory (default: log_dir/archive/)
	LogFileMode         string `toml:"log_file_mode"          default:"0600"  desc:"octal file permissions for log files"` // octal file permissions for log files (default "0600")
}

// TTSConfig describes a text-to-speech provider entry.
// Multiple entries are supported via [[tts]]; first entry is the default.
type TTSConfig struct {
	ID             string  `toml:"id"`              // lookup key for agent overrides
	Format         string  `toml:"format"`          // "openai" or "edge-tts"
	Endpoint       string  `toml:"endpoint"`        // API URL (ignored for edge-tts)
	Model          string  `toml:"model"`           // model name (ignored for edge-tts)
	Voice          string  `toml:"voice"`           // voice name (format-specific)
	Rate           float64 `toml:"rate"`            // speed multiplier: 1.0 = normal, 0 = omit
	Secret         string  `toml:"secret"`          // secret name in secrets.toml (optional, fallback: hostname)
	Command        string            `toml:"command"`         // binary for edge-tts (default: "edge-tts")
	ResponseFormat string            `toml:"response_format"` // audio format: "mp3", "wav", etc. (default: "wav")
	Replacements   map[string]string `toml:"replacements"`    // word replacements applied before synthesis (e.g. "foci" = "foki")
}

// STTConfig describes a speech-to-text provider entry.
// Multiple entries are supported via [[stt]]; first entry is the default.
type STTConfig struct {
	ID       string `toml:"id"`       // lookup key for agent overrides
	Format   string `toml:"format"`   // "openai" (only supported format currently)
	Endpoint string `toml:"endpoint"` // API URL
	Model    string `toml:"model"`    // model name
	Secret       string            `toml:"secret"`       // secret name in secrets.toml (optional, fallback: hostname)
	Replacements map[string]string `toml:"replacements"` // word replacements applied after transcription (e.g. "foki" = "foci")
}

type BitwardenConfig struct {
	Enabled         bool   `toml:"enabled"`
	RefreshInterval string `toml:"refresh_interval" default:"15m"`                       // how often to refresh item metadata (default "15m")
	SecretTTL       string `toml:"secret_ttl"       default:"30m"`                       // how long unlocked values stay cached (default "30m")
	SessionFile     string `toml:"session_file"     default:"/home/bitwarden/.bw_session"` // path to BW session file read by bitwarden user (default "/home/bitwarden/.bw_session")
	CleanupInterval string `toml:"cleanup_interval" default:"1m"`                        // how often to purge expired values (default "1m")
}


// PermissionsConfig controls foci-level auto-approval of delegated backend
// permission requests. Rules from global [permissions] and per-agent
// [[agents]].permissions are combined (union) — both sets apply.
// All fields are pointer/slice types for Merge-based resolution.
type PermissionsConfig struct {
	AutoApprove               []string `toml:"auto_approve"`                                                                                        // glob patterns (e.g. "Bash:git *") to auto-approve without prompting
	AutoApproveCommonReadonly *bool    `toml:"auto_approve_common_readonly"  default:"true"  desc:"auto-approve common read-only tools and commands"` // enable built-in read-only tool/command allowlist
	AutoApproveCommonSafeWrite *bool   `toml:"auto_approve_common_safe_write" default:"false" desc:"auto-approve common side-effecting commands (curl, wget, mkdir, touch)"` // enable built-in safe-write allowlist (default false — not path-scoped)
	// PromptTTL is how long an unanswered interactive prompt (permission
	// request, AskUserQuestion) stays live before it auto-expires. On expiry
	// the prompt is resolved as a denial/cancel so the waiting backend doesn't
	// orphan, and its message is edited to show it expired. Aligns with the
	// delegated-backend idle_timeout default. Parsed via time.ParseDuration.
	PromptTTL string `toml:"prompt_ttl" default:"24h" desc:"lifetime of an unanswered permission/question prompt before it auto-denies"`
}

// AutoApproveCommonReadonlyEnabled returns the resolved value (default: true).
func (p PermissionsConfig) AutoApproveCommonReadonlyEnabled() bool {
	if p.AutoApproveCommonReadonly != nil {
		return *p.AutoApproveCommonReadonly
	}
	return true
}

// AutoApproveCommonSafeWriteEnabled returns the resolved value (default: false).
func (p PermissionsConfig) AutoApproveCommonSafeWriteEnabled() bool {
	if p.AutoApproveCommonSafeWrite != nil {
		return *p.AutoApproveCommonSafeWrite
	}
	return false
}

type EnvironmentConfig struct {
	Enabled  *bool   `toml:"enabled"    default:"true"         desc:"inject environment block"` // inject environment block as first system block (default true)
	DocsPath *string `toml:"docs_path"  default:"shared/docs"  desc:"path to platform docs directory"` // path to platform docs directory; relative paths resolve against $HOME
}

type SkillsConfig struct {
	Dir string `toml:"dir"` // shared skills directory (default: $home/shared/skills/)
}

type ResourcesConfig struct {
	MemoryGuardEnabled        *bool    `toml:"memory_guard_enabled"        default:"true" desc:"enable system memory guard"` // enable system memory guard (default true)
	MemoryGuardInterval       string   `toml:"memory_guard_interval"       default:"60s"  desc:"memory guard check interval" type:"duration"` // check interval (default "60s")
	MemoryWarnPercent         *int     `toml:"memory_warn_percent"         default:"25"   desc:"warn threshold as %% of total RAM"` // warn threshold as % of total RAM (default 25)
	MemoryKillPercent         *int     `toml:"memory_kill_percent"         default:"40"   desc:"kill threshold as %% of total RAM"` // kill threshold as % of total RAM (default 40)
	MemoryPressureThreshold   *float64 `toml:"memory_pressure_threshold"   default:"10"   desc:"PSI avg10 threshold before acting"` // PSI avg10 threshold to require before acting (default 10.0)
	GoroutineMonitorInterval  string   `toml:"goroutine_monitor_interval"  default:"60s"  desc:"goroutine count check interval" type:"duration"` // goroutine count check interval (default "60s")
	GoroutineMonitorThreshold int      `toml:"goroutine_monitor_threshold"                desc:"goroutine count warning threshold (0=auto)"` // warn when goroutine count exceeds this (0 = auto: 30 + 25×agents + 5×telegram_bots)
}

// BrowserConfig holds configuration for the browser automation tool.
// All fields are pointer types for Merge-based resolution (per-agent → global).
// TOML: [browser] globally, [[agents]].browser per-agent.
type BrowserConfig struct {
	Enabled        *bool    `toml:"enabled"         default:"true" desc:"enable browser tool"` // enable browser tool
	Headless       *bool    `toml:"headless"        default:"true" desc:"run headless"` // run headless
	TimeoutSec     *int     `toml:"timeout_sec"     default:"30"   desc:"page operation timeout in seconds"` // page operation timeout in seconds
	UserDataDir    *string  `toml:"user_data_dir"                  desc:"Chrome user data dir (empty = temp profile)"` // Chrome user data dir (empty = temp profile)
	ExecutablePath *string  `toml:"executable_path"                desc:"Chrome executable path (empty = auto-detect)"` // Chrome executable path (empty = auto-detect)
	DOMStableSec   *float64 `toml:"dom_stable_sec"  default:"1"    desc:"DOM stability wait interval in seconds"` // DOM stability wait interval in seconds
	DOMStableDiff  *float64 `toml:"dom_stable_diff" default:"0.2"  desc:"DOM stability diff threshold"` // DOM stability diff threshold
}

type ToolsConfig struct {
	SummaryConfig // global summary/tool-result defaults (resolved via Merge with per-agent)
	ToolConfig    // global tool behavioral defaults (resolved via Merge with per-agent)

	TempDir                 string   `toml:"temp_dir"                   default:"/tmp/foci/tool-results"` // where to write large tool results (default /tmp/foci/tool-results)
	TmuxCols                int      `toml:"tmux_cols"                  default:"300"       desc:"tmux window columns"` // tmux window columns on start (default 300)
	TmuxRows                int      `toml:"tmux_rows"                  default:"30"        desc:"tmux window rows"` // tmux window rows on start (default 30)
	ExecDefaultTimeout      int      `toml:"exec_default_timeout"       default:"30"        desc:"default timeout for exec in seconds"` // default timeout for exec commands in seconds (default 30)
	TmuxCommandTimeout      string   `toml:"tmux_command_timeout"       default:"5s"`                    // timeout for tmux control commands (default "5s")
	WebFetchTimeout         string   `toml:"web_fetch_timeout"          default:"30s"       desc:"HTTP timeout for web fetch" type:"duration"` // HTTP timeout for web fetch (default "30s")
	WebFetchMaxBytes        int      `toml:"web_fetch_max_bytes"        default:"1048576"`               // max bytes to read from web fetch (default 1048576 = 1MB)
	WebSearchTimeout        string   `toml:"web_search_timeout"         default:"15s"       desc:"HTTP timeout for web search" type:"duration"` // HTTP timeout for web search (default "15s")
	ToolCallPreviewChars    int      `toml:"tool_call_preview_chars"    default:"450"       desc:"max chars for tool call preview"` // max chars for tool call param preview in Telegram (default 450)
	TmuxMemoryCheckInterval string   `toml:"tmux_memory_check_interval" default:"5m"`                    // how often to check tmux RSS (default "5m", "0" disables)
	TmuxMemoryWarn          string   `toml:"tmux_memory_warn"           default:"10%"`                   // warn threshold as % of RAM or absolute (default "10%")
	TmuxMemoryCritical      string   `toml:"tmux_memory_critical"       default:"20%"`                   // critical threshold (default "20%")
	TmuxMemoryKill          string   `toml:"tmux_memory_kill"           default:"30%"`                   // kill threshold (default "30%")
	WebSearchMaxUses        int      `toml:"web_search_max_uses"`        // max searches per API call (0 = unlimited)
	WebSearchAllowedDomains []string `toml:"web_search_allowed_domains"` // domain whitelist (mutually exclusive with blocked)
	WebSearchBlockedDomains []string `toml:"web_search_blocked_domains"` // domain blacklist
	WebFetchMaxUses         int      `toml:"web_fetch_max_uses"`         // max fetches per API call (0 = unlimited)
	WebFetchAllowedDomains  []string `toml:"web_fetch_allowed_domains"`  // domain whitelist
	WebFetchBlockedDomains  []string `toml:"web_fetch_blocked_domains"`  // domain blacklist
}

type MessageTransform struct {
	Find    string `toml:"find"`    // regex pattern to match
	Replace string `toml:"replace"` // replacement string (supports $1, $2, etc.)
}

type BlockedPath struct {
	Path   string `toml:"path"`   // directory or file prefix to block
	Rebuke string `toml:"rebuke"` // message returned when write/edit is attempted
}

type CommandConfig struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Script      string `toml:"script"`
	Timeout     int    `toml:"timeout"` // seconds, default 10
}

// DefaultsConfig provides global defaults for agent-specific fields.
// All config groups use Merge[T] for resolution at use time.
type DefaultsConfig struct {
	Notify   NotifyConfig    `toml:"notify"`
	Display  DisplayConfig   `toml:"display"`
	Nudge    NudgeConfig     `toml:"nudge"`
	Voice    VoiceConfig     `toml:"voice"`
	Loop     AgentLoopConfig `toml:"loop"`
	Behavior BehaviorConfig  `toml:"behavior"`
	System   SystemConfig    `toml:"system"`
}


// EndpointConfig describes a model API endpoint.
type EndpointConfig struct {
	// Single-format endpoints:
	Format string `toml:"format"` // "anthropic", "openai", or "gemini"
	URL    string `toml:"url"`    // base URL (empty = SDK/library default)

	// Multi-format endpoints (overrides Format+URL when set):
	AnthropicURL string `toml:"anthropic_url"`
	OpenAIURL    string `toml:"openai_url"`
	GeminiURL    string `toml:"gemini_url"`

	// Shared:
	APIKey      string `toml:"api_key"`      // secret name in secrets store (e.g. "openrouter.api_key")
	HTTPTimeout string `toml:"http_timeout"` // Go duration (default "120s")
}

// SupportsFormat reports whether this endpoint supports the given wire format.
func (e EndpointConfig) SupportsFormat(f string) bool {
	switch f {
	case "anthropic":
		return e.AnthropicURL != "" || e.Format == "anthropic"
	case "openai":
		return e.OpenAIURL != "" || e.Format == "openai"
	case "gemini":
		return e.GeminiURL != "" || e.Format == "gemini"
	}
	return false
}

// URLForFormat returns the base URL for the given wire format, or empty for SDK default.
func (e EndpointConfig) URLForFormat(f string) string {
	switch f {
	case "anthropic":
		if e.AnthropicURL != "" {
			return e.AnthropicURL
		}
	case "openai":
		if e.OpenAIURL != "" {
			return e.OpenAIURL
		}
	case "gemini":
		if e.GeminiURL != "" {
			return e.GeminiURL
		}
	}
	return e.URL
}

// KeepaliveConfig controls the cache keepalive timer.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type KeepaliveConfig struct {
	Enabled  *bool   `toml:"enabled"                  desc:"enable keepalive timer"` // enable keepalive timer
	Interval *string `toml:"interval" default:"55m"   desc:"time since cache last warmed" type:"duration"` // time since cache last warmed before firing
	Prompt   *string `toml:"prompt"                   desc:"keepalive prompt file path"` // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
}

// ReflectionConfig controls the periodic reflection pass, which captures both
// factual memory (memory files) and procedural knowledge (autogenerated skills)
// from recent session activity. All fields are pointer types for Merge-based
// resolution (per-agent → global).
type ReflectionConfig struct {
	IntervalEnabled       *bool   `toml:"interval_enabled"       default:"true" desc:"periodic reflection pass on timer"` // periodic reflection on timer
	Interval              *string `toml:"interval"               default:"1h"   desc:"time between reflection passes" type:"duration"` // time between reflections
	IntervalPrompt        *string `toml:"interval_prompt"                       desc:"interval reflection prompt file path"` // prompt override (nil = embedded, "none" = disabled)
	ConsolidationEnabled  *bool   `toml:"consolidation_enabled"  default:"true" desc:"curate MEMORY.md periodically"` // curate MEMORY.md periodically
	ConsolidationInterval *string `toml:"consolidation_interval" default:"20h"  desc:"min time between consolidations" type:"duration"` // min time between consolidations
	ConsolidationPrompt   *string `toml:"consolidation_prompt"                  desc:"consolidation prompt file path"` // prompt override (nil = embedded, "none" = disabled)
	SessionEndEnabled     *bool   `toml:"session_end_enabled"    default:"true" desc:"run reflection on /reset and reclaim"` // reflect on /reset and reclaim
	SessionEndPrompt      *string `toml:"session_end_prompt"                    desc:"session end reflection prompt file path"` // prompt override (nil = embedded, "none" = disabled)
	CompactionEnabled     *bool   `toml:"compaction_enabled"     default:"true" desc:"reflection before compaction"` // reflect before compaction
	CompactionPrompt      *string `toml:"compaction_prompt"                     desc:"compaction reflection prompt file path"` // prompt override (nil = embedded, "none" = disabled)
	BackendQuietPeriod    *string `toml:"backend_quiet_period"   default:"5m"   desc:"min idle time before reflection in backend mode" type:"duration"` // min idle before firing in backend mode
}

// BackgroundConfig controls the mana-gated background work timer.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type BackgroundConfig struct {
	Enabled  *bool   `toml:"enabled"                  desc:"enable background work timer"` // enable background work timer
	Interval *string `toml:"interval" default:"15m"   desc:"time since last interaction before firing" type:"duration"` // time since last interaction before firing
	Prompt   *string `toml:"prompt"                   desc:"background work prompt file path"` // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
}

// DebugConfig holds developer/debugging knobs that can be configured at
// any scope level. Resolution follows the 5-level cascade via Merge.
// All fields are nillable so nil means "not set, inherit from wider scope."
type DebugConfig struct {
	LogAPIKeySuffix      *bool `toml:"log_api_key_suffix"      desc:"log last 4 chars of API keys on provider calls"` // log last 4 chars of API keys on each provider call (default false)
	MessagesInLog        *bool `toml:"messages_in_log"         desc:"log user message content"` // log user message content to event log (default false for privacy)
	CacheBustDetect      *bool `toml:"cache_bust_detect"       default:"false" desc:"alert on cache_read drop"` // alert when cache_read drops >50% vs previous request
	CacheBustIdleMinutes *int  `toml:"cache_bust_idle_minutes" default:"10"    desc:"suppress cache bust alert if idle > N minutes"` // suppress cache bust alert if session idle > N minutes (default 10)

	// EnablePprof exposes the net/http/pprof endpoints under /debug/pprof/*.
	// Off by default: they allow CPU/heap profiling and goroutine dumps, so they
	// are gated behind an explicit opt-in even though the HTTP server is
	// auth-gated. Process-global (top-level [debug] section).
	EnablePprof *bool `toml:"enable_pprof" default:"false" desc:"expose /debug/pprof/* profiling endpoints"`

	// Per-package "extra" verbose logging. Each switches on investigation-grade
	// logging for one package, tagged "xtra:<package>" in the log (grep
	// "xtra:ccstream", or "xtra:" for all). Process-global (applied once at
	// startup from the top-level [debug] section); default off. See log.Extra.
	ExtraCcstreamLogging *bool `toml:"extra_ccstream_logging" default:"false" desc:"verbose ccstream logs tagged xtra:ccstream"` // verbose ccstream turn/steer logging
	ExtraTelegramLogging *bool `toml:"extra_telegram_logging" default:"false" desc:"verbose telegram logs tagged xtra:telegram"` // verbose telegram poll/transport logging
	ExtraInboxLogging    *bool `toml:"extra_inbox_logging"    default:"false" desc:"verbose inbox logs tagged xtra:inbox"`       // verbose inbox routing/gate logging
}

type Config struct {
	DataDir            string                       `toml:"data_dir"`  // directory for databases, sessions, state (default: $HOME/data)
	Notify             NotifyConfig                  `toml:"notify"`      // global notification defaults
	Display            DisplayConfig                 `toml:"display"`     // global display defaults
	Nudge              NudgeConfig                   `toml:"nudge"`       // global nudge defaults
	Voice              VoiceConfig                   `toml:"voice"`       // global voice defaults
	AgentLoop          AgentLoopConfig               `toml:"agent_loop"`  // global agent loop defaults
	Behavior           BehaviorConfig                `toml:"behavior"`    // global behavior defaults
	System             SystemConfig                  `toml:"system"`      // global system defaults
	Groups             GroupsConfig                  `toml:"groups"`      // model group assignments and fallbacks
	Models             map[string]ModelConfig        `toml:"models"`    // named model definitions with per-model settings
	Endpoints          map[string]EndpointConfig     `toml:"endpoints"` // named API endpoints (built-in: anthropic, gemini, openai, openrouter)
	Agents             []AgentConfig             `toml:"agents"`    // multi-agent: array of agents
	Anthropic          AnthropicConfig           `toml:"anthropic"`
	Platforms          []PlatformConfig          `toml:"platforms"`
	Sessions           SessionsConfig            `toml:"sessions"`
	Memory             MemoryConfig              `toml:"memory"`
	Database           DatabaseConfig            `toml:"database"`
	HTTP               HTTPConfig                `toml:"http"`
	Logging            LoggingConfig             `toml:"logging"`
	TTS                []TTSConfig               `toml:"tts"`
	STT                []STTConfig               `toml:"stt"`
	Bitwarden          BitwardenConfig           `toml:"bitwarden"`
	Mana               ManaConfig                `toml:"mana"`
	Environment        EnvironmentConfig         `toml:"environment"`
	Skills             SkillsConfig              `toml:"skills"`
	Resources          ResourcesConfig           `toml:"resources"`
	Debug              DebugConfig               `toml:"debug"`
	Tools              ToolsConfig               `toml:"tools"`
	Browser            BrowserConfig             `toml:"browser"`
	Keepalive          KeepaliveConfig           `toml:"keepalive"`
	Background         BackgroundConfig          `toml:"background"`
	Reflection         ReflectionConfig          `toml:"reflection"`
	Permissions        PermissionsConfig         `toml:"permissions"`
	CCBackend          CCBackendConfig           `toml:"cc_backend"`          // shared defaults for Claude Code delegator backends
	Commands           []CommandConfig           `toml:"commands"`
	MessageTransforms  []MessageTransform        `toml:"message_transforms"`   // regex find/replace rules applied to inbound messages
	BlockedPaths       []BlockedPath             `toml:"blocked_paths"`        // path prefixes that write/edit tools refuse (with rebuke message)
	FileMode           string                    `toml:"file_mode"            default:"0640"` // octal file permissions for workspace/content files (default "0640")
	WelcomeFile        string                    `toml:"welcome_file"         default:"data/WELCOME.md"` // path to welcome/changelog file injected on startup (e.g. /home/foci/WELCOME.md)
	Timezone           string                    `toml:"timezone"`             // IANA timezone for timestamps (e.g. "Europe/Athens", "UTC", "Local"); empty = machine local
	SkipSecurityChecks bool                      `toml:"skip_security_checks"` // if true, skip startup security checks for secrets.toml
	DefinedKeys        map[string]bool           `toml:"-"`                    // keys explicitly set in TOML file (populated by Load)
	UndefinedKeys      []string                  `toml:"-"`                    // unrecognised TOML keys (populated by Load, logged by caller)
}
