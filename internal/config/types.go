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
	InjectionAll    InjectionLevel = "all"    // inject WARN + ERROR
	InjectionErrors InjectionLevel = "errors" // inject ERROR only
	InjectionOff    InjectionLevel = "off"    // disabled
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
	EnableKeepalive *bool         `toml:"enable_keepalive"` // nil=auto-detect, true/false=explicit
	CacheTTL        string        `toml:"cache_ttl"`        // cache TTL: Go duration, empty=auto-detect (Anthropic: "5m"/"1h"; Gemini: any duration)
	CacheStrategy   string        `toml:"cache_strategy"`   // cache marker strategy: "auto" or "explicit" (Anthropic only, default "auto")
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

	// BackgroundTaskMaxAge bounds how long a spawned background task (an
	// Agent-tool subagent or a run_in_background Bash) stays tracked without a
	// completion signal before the prune drops it. The prune is the unwedge
	// backstop for the pending-work gate that holds system injects while
	// background work is outstanding (spec §4): a task whose completion
	// notification is missed can't hold injects forever. Empty → 30m. Set well
	// beyond any real background job's runtime.
	BackgroundTaskMaxAge string `toml:"background_task_max_age" desc:"Max time a background task can run before being dropped from tracking if no completion signal arrives, freeing up any reminders waiting on it. Empty = 30m" type:"duration"`
}

// GroupsConfig assigns named models to groups and call sites.
// Groups is populated from top-level string keys in [groups] by load.go
// (not decoded by TOML directly since the section mixes string keys with sub-tables).
// Users can define arbitrary groups; "powerful" is required, "fast"/"cheap" default to it.
type GroupsConfig struct {
	Groups    map[string]string `toml:"-"`         // group name → model (populated by load.go from TOML metadata)
	Calls     map[string]string `toml:"calls"`     // call site → group overrides
	Fallbacks map[string]string `toml:"fallbacks"` // model → fallback model
}

type AgentConfig struct {
	ID              string `toml:"id"`
	Name            string `toml:"name"`             // human-readable name (e.g. "Clutch"); used in voice endpoint agent list
	Emoji           string `toml:"emoji"`            // emoji for agent (e.g. "🥔"); used in voice endpoint agent list
	DefaultPlatform string `toml:"default_platform"` // per-agent override of the global default_platform
	Workspace       string `toml:"workspace"`
	// Avatar is an image file for the agent, served to the native app. An
	// absolute path, or a path relative to the foci home dir, is used as-is
	// (resolved via ResolvePath). When empty, it is auto-detected at load from
	// $workspace/avatar.{png,jpg,jpeg,webp,gif} then $workspace/.data/avatar.{ext}.
	// After Load() this holds "" (no avatar) or an absolute path to an existing file.
	Avatar string `toml:"avatar"`

	Memory    MemoryConfig     `toml:"memory"`    // per-agent memory overrides (sources combined with global [memory])
	Platforms []PlatformConfig `toml:"platforms"` // per-agent platform configurations

	// Per-agent overrides — resolved via Merge at use time
	// (e.g. config.Merge(acfg.Nudge, cfg.Defaults.Nudge)).
	Notify   NotifyConfig    `toml:"notify"`   // overrides from [defaults.notify]
	Display  DisplayConfig   `toml:"display"`  // overrides from [defaults.display]
	Nudge    NudgeConfig     `toml:"nudge"`    // overrides from [defaults.nudge]
	Voice    VoiceConfig     `toml:"voice"`    // overrides from [defaults.voice]
	Loop     AgentLoopConfig `toml:"loop"`     // overrides from [defaults.loop]
	Behavior BehaviorConfig  `toml:"behavior"` // overrides from [defaults.behavior]
	System   SystemConfig    `toml:"system"`   // overrides from [defaults.system]

	Sessions    AgentSessionsOverride `toml:"sessions"`    // overrides from [sessions]
	Tools       AgentToolsOverride    `toml:"tools"`       // overrides from [tools]
	Debug       DebugConfig           `toml:"debug"`       // overrides from [debug]
	Environment EnvironmentConfig     `toml:"environment"` // overrides from [environment]
	Browser     BrowserConfig         `toml:"browser"`     // overrides from [browser]
	Keepalive   KeepaliveConfig       `toml:"keepalive"`   // overrides from [keepalive]
	Background  BackgroundConfig      `toml:"background"`  // overrides from [background]
	Reflection  ReflectionConfig      `toml:"reflection"`  // overrides from [reflection]
	Scheduler   SchedulerConfig       `toml:"scheduler"`   // overrides from [scheduler]
	Maintenance MaintenanceConfig     `toml:"maintenance"` // overrides from [maintenance]
	Groups      GroupsConfig          `toml:"groups"`      // overrides from [groups]
	Permissions PermissionsConfig     `toml:"permissions"` // overrides from [permissions]

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
	MaxSystemPromptFile             *int    `toml:"max_system_prompt_chars_file"  desc:"Logs a server-side warning if any single system prompt file, eg a character file, exceeds this many characters. Overrides the global sessions value for this agent"`
	MaxSystemPromptTotal            *int    `toml:"max_system_prompt_chars_total" desc:"Logs a server-side warning if this agent's combined system prompt exceeds this many characters total. Overrides the global sessions value for this agent"`
	EphemeralRetentionDays          *int    `toml:"ephemeral_retention_days"      hot:"event" desc:"Auto-deletes old transcripts from short-lived internal sessions, eg reflection, keepalive, background tasks, after this many days. Normal chat history is untouched. 0 = never delete" min:"0"`
}

// EffectiveEphemeralRetentionDays returns the per-agent ephemeral-session
// retention (days) if the agent overrides it, else the global [sessions] value.
func (a AgentSessionsOverride) EffectiveEphemeralRetentionDays(global int) int {
	if a.EphemeralRetentionDays != nil {
		return *a.EphemeralRetentionDays
	}
	return global
}

// EffectiveMaxSystemPromptFile returns the per-agent per-file char warning
// threshold if the agent overrides it, else the global [sessions] value.
func (a AgentSessionsOverride) EffectiveMaxSystemPromptFile(global int) int {
	if a.MaxSystemPromptFile != nil {
		return *a.MaxSystemPromptFile
	}
	return global
}

// EffectiveMaxSystemPromptTotal returns the per-agent total system-prompt char
// warning threshold if the agent overrides it, else the global [sessions] value.
func (a AgentSessionsOverride) EffectiveMaxSystemPromptTotal(global int) int {
	if a.MaxSystemPromptTotal != nil {
		return *a.MaxSystemPromptTotal
	}
	return global
}

// AgentToolsOverride groups the config groups whose global home is [tools].
type AgentToolsOverride struct {
	ToolConfig
	SummaryConfig
}

// CompactionConfig holds compaction settings. Embed in SessionsConfig (global) and AgentConfig (per-agent).
type CompactionConfig struct {
	CompactionThreshold        *float64 `toml:"compaction_threshold"  desc:"Fraction of the context window (0 to 1) that must fill before old messages are summarized. Leave unset for a curve that waits longer on larger windows" min:"0" max:"1"`
	CompactionSummaryPrompt    *string  `toml:"compaction_summary_prompt"             desc:"Path to a file containing the prompt used to ask the model to summarize old messages during compaction"`
	CompactionHandoffMsg       *string  `toml:"compaction_handoff_msg"                desc:"Path to a file containing the message shown to the model right after compaction to help it pick back up"`
	CompactionPreserveMessages *int     `toml:"compaction_preserve_messages"          desc:"Number of most recent messages kept word-for-word instead of being summarized when compaction runs. 0 = summarize everything. Default 25" min:"0"`
	FacetNoCompact             *bool    `toml:"facet_no_compact"                      desc:"Facets are short-lived side sessions branched off the main chat. When true, they are never compacted since they do not last long (default true)"`
	ReloadOnCompact            *bool    `toml:"reload_on_compact"                     desc:"For Claude Code-backed agents, restarts the session after compaction so edits to character or skill files since it started take effect (default true)"`
}

// NudgeConfig holds nudge system settings.
// Global: [nudge], per-agent: [[agents]].nudge.*
type NudgeConfig struct {
	NudgeEnable                     *bool   `toml:"nudge_enable"                      default:"true"  desc:"Turns on nudges, short reminder messages injected while the agent is working to help keep it on track"`
	NudgeAutoExtract                *bool   `toml:"nudge_auto_extract"                default:"true"  desc:"Has an LLM automatically scan character files for behavioral rules to turn into nudges, instead of writing them by hand"`
	NudgeCooldown                   *int    `toml:"nudge_cooldown"                                    desc:"Minimum number of tool calls that must pass before the same nudge reminder can fire again, so it does not repeat every batch. Default 5"`
	NudgeMaxPerBatch                *int    `toml:"nudge_max_per_batch"                               desc:"Maximum number of different nudge reminders that can be injected after a single batch of tool calls. Default 1"`
	NudgePreAnswerGate              *bool   `toml:"nudge_pre_answer_gate"                             desc:"Before ending a turn, gives the agent one chance to reconsider against pre-answer reminders once it has made a few tool calls. Off by default"`
	NudgePreAnswerMinTools          *int    `toml:"nudge_pre_answer_min_tools"          default:"2"   desc:"Minimum tool calls in the current turn before the pre-answer verification gate is allowed to trigger"`
	NudgeDefaultEnable              *bool   `toml:"nudge_default_enable"              default:"true"  desc:"Turns on the built-in reminders that tell the agent which tools and skills are available"`
	NudgeDefaultFrequency           *int    `toml:"nudge_default_frequency"             default:"50"  desc:"Number of turns between built-in tool and skill reminders"`
	NudgeDefaultScratchpadFrequency *int    `toml:"nudge_default_scratchpad_frequency"                desc:"Number of turns between reminders to review the agent's scratchpad notes. 0 disables it. Default 20"`
	NudgeDefaultBraindeadThreshold  *int    `toml:"nudge_default_braindead_threshold"                 desc:"Number of tool calls in a row within one turn before the agent is warned to stop and check it is still on track. 0 disables this check"`
	NudgeDefaultBraindeadPrompt     *string `toml:"nudge_default_braindead_prompt"`
}

// SummaryConfig holds tool result summarisation settings.
// Embed in ToolsConfig (global) and AgentToolsOverride (per-agent).
type SummaryConfig struct {
	MaxResultChars       *int  `toml:"max_result_chars"                       desc:"When a tool result, eg command output or a fetched page, exceeds this many characters, the full result is saved to a file instead of sent to the model. Default 15000"`
	MaxSummaryChars      *int  `toml:"max_summary_chars"                      desc:"Oversized tool results up to this many characters get an automatic AI summary instead of just a link to the saved file; larger ones skip it. Default 300000"`
	AutoSummarise        *bool `toml:"auto_summarise"         default:"true"  desc:"Automatically generates a short AI summary of oversized tool results instead of leaving just a pointer to the saved file"`
	SummaryContextTurns  *int  `toml:"summary_context_turns"                  desc:"Number of recent conversation turns included as context when auto-summarizing an oversized tool result. Default 5"`
	SummaryContextChars  *int  `toml:"summary_context_chars"                  desc:"Maximum characters of conversation context sent to the model when auto-summarizing an oversized tool result. Default 6000"`
	MaxSummaryInputChars *int  `toml:"max_summary_input_chars"                desc:"Maximum characters of the oversized tool result itself fed into the summarizer; the full result is still saved to disk regardless. Default 100000"`
	MaxImagePixels       *int  `toml:"max_image_pixels"                       desc:"Images larger than this many total pixels, width times height, are resized down before being sent to the model. 0 disables downscaling; default is roughly 1920x1080"`
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

// DefaultMaxFileReadBytes is the fallback cap the read/edit tools apply to a
// file's size before loading it, when [tools] max_file_read_bytes is unset
// (mirrors the ToolConfig default tag). Guards against large-file OOM.
const DefaultMaxFileReadBytes int64 = 50 << 20 // 50 MiB

// VoiceConfig holds TTS/STT settings.
// Global: [voice], per-agent: [[agents]].voice.*
type VoiceConfig struct {
	TTS                  *string           `toml:"tts"      hot:"event" desc:"Which text-to-speech provider to use for voice replies"`
	STT                  *string           `toml:"stt"      desc:"Which speech-to-text provider to use for transcribing voice messages"`
	TTSRate              *float64          `toml:"tts_rate"  hot:"event" desc:"Speeds up or slows down text-to-speech playback. 1.0 is normal speed, higher is faster, lower is slower. 0 is treated as 1.0"`
	TTSReplacements      map[string]string `toml:"tts_replacements"`
	STTReplacements      map[string]string `toml:"stt_replacements"`
	MaxFrameBytes        *int              `toml:"max_frame_bytes" default:"1048576"  scope:"global" desc:"Maximum size in bytes of a single incoming voice websocket message; larger frames are rejected (1 MiB)"`
	MaxAudioBytes        *int              `toml:"max_audio_bytes" default:"52428800" scope:"global" desc:"Maximum total bytes of audio buffered for one voice turn before it is cut off (50 MiB)"`
	MaxConcurrentTurns   *int              `toml:"max_concurrent_turns" default:"4" scope:"global" desc:"Maximum number of voice turns, speech-to-text plus agent reply plus text-to-speech, that can be in progress at once on one connection"`
	HTTPTimeout          *string           `toml:"http_timeout" default:"60s" scope:"global" desc:"How long to wait for the speech-to-text or text-to-speech provider to respond before giving up"`
	HTTPMaxResponseBytes *int              `toml:"http_max_response_bytes" default:"67108864" scope:"global" desc:"Maximum size in bytes of a response from the speech-to-text or text-to-speech provider before it is rejected (64 MiB)"`
}

// AgentLoopConfig holds settings consumed by agent.HandleTurn().
// Global: [agent_loop], per-agent: [[agents]].agent_loop.*
type AgentLoopConfig struct {
	MaxOutputTokens               *int    `toml:"max_output_tokens"                desc:"Maximum number of tokens the model may generate in a single reply. Higher allows longer replies but costs more and takes longer. Default 16384"`
	MaxToolLoops                  *int    `toml:"max_tool_loops"                   hot:"turn" desc:"Maximum tool calls allowed in a single turn; once reached, further tool calls are refused and the turn is forced to end. Default 100"`
	DuplicateMessages             *bool   `toml:"duplicate_messages"               desc:"Repeats your message text twice in the prompt sent to the model, a technique that can improve instruction-following on some models. Off by default"`
	BatchPartialAssistantMessages *bool   `toml:"batch_partial_assistant_messages"  desc:"When the agent sends text between tool calls, combine it all into one message at the end of the turn instead of sending each piece right away. Off by default"`
	BatchPartialJoiner            *string `toml:"batch_partial_joiner"             desc:"Text inserted between chunks when batch_partial_assistant_messages combines them into one message. Empty joins them with nothing in between"`
}

// BehaviorConfig holds agent behavioral settings.
// Global: [behavior], per-agent: [[agents]].behavior.*
type BehaviorConfig struct {
	SteerMode             *bool    `toml:"steer_mode"               default:"true"  desc:"Lets a message you send while the agent is mid-turn redirect it at the next tool call, instead of waiting for the turn to finish"`
	GroupThrottle         *string  `toml:"group_throttle"                            desc:"In group chats, buffers messages that do not mention the agent for this long, then delivers them together. A mention flushes the buffer immediately. Empty disables it" type:"duration"`
	TurnLockWarnThreshold *string  `toml:"turn_lock_warn_threshold" default:"3m"    desc:"Writes a warning to the server log if a turn waits longer than this for the previous turn on the same session to finish. Diagnostic only, not shown to users" type:"duration"`
	EnableStopAliases     *bool    `toml:"enable_stop_aliases"      default:"true"  desc:"Lets extra words such as wait also work as aliases for stop, which cancels the agent's current turn"`
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
	ExecAutoBackground  *int    `toml:"exec_auto_background"  default:"10"        desc:"Seconds an exec command runs before it's automatically moved to the background so the agent isn't blocked waiting on it"`
	MaxConcurrentSpawns *int    `toml:"max_concurrent_spawns" default:"3"         desc:"Maximum number of spawned subagent sessions that can run at once; further spawn requests queue until a slot frees up"`
	ExploreMaxDepth     *int    `toml:"explore_max_depth"     default:"100"       desc:"Maximum tool-call iterations an explore-type spawned subagent can make before it's forced to stop and return results"`
	MaxUploadFileSize   *int64  `toml:"max_upload_file_size"  default:"52428800"  desc:"Maximum file size, in bytes, the upload tool will accept (default 50 MiB); larger files are rejected"`                                                // 50MB
	MaxFileReadBytes    *int64  `toml:"max_file_read_bytes"   default:"52428800"  desc:"Maximum file size, in bytes, the read and edit tools will load (default 50 MiB); larger files must be read with an offset/limit range"`               // 50MB
	HTTPMaxSpillBytes   *int64  `toml:"http_max_spill_bytes"  default:"52428800"  desc:"Maximum bytes of an http_request response kept inline in the conversation (default 50 MiB); anything beyond that is saved to a file on disk instead"` // 50MB
	TmuxAutopilot       *bool   `toml:"tmux_autopilot"        default:"true"      desc:"Automatically watch a tmux pane's output after you send it input, and stop watching once it goes quiet, without needing manual watch/unwatch calls"`
	TmuxWatchThreshold  *string `toml:"tmux_watch_threshold"  default:"30s"       desc:"How long a tmux pane must produce no new output before a watch is considered idle and reports back (default 30s)" type:"duration"`
	TmuxSessionTTL      *string `toml:"tmux_session_ttl"      default:"24h"       desc:"Idle tmux sessions are automatically killed after being inactive this long (default 24h); set to 0 to disable auto-kill" type:"duration"`
	SearchProvider      *string `toml:"search_provider"       default:"brave"     desc:"Backend used for web search: brave calls the Brave Search API, anthropic uses Anthropic's built-in web search" choices:"brave,anthropic"`
	FetchProvider       *string `toml:"fetch_provider"        default:"builtin"   desc:"Backend used to fetch web pages: builtin fetches directly from this server, anthropic uses Anthropic's hosted fetch tool" choices:"anthropic,builtin"`
	TodoFormat          *string `toml:"todo_format"                                desc:"How the agent's todo list is rendered in chat: lines shows a simple bullet list, table shows a formatted table" choices:"lines,table"`
}

type AnthropicConfig struct {
	CCExpiryThreshold string `toml:"cc_expiry_threshold" default:"5m"  desc:"How far ahead of expiry the server refreshes cached Claude Code login credentials (~/.claude/.credentials.json), an alternate way to authenticate to Anthropic (default 5m)" type:"duration"`
}

// DisplayConfig holds display-related settings that can be set at any level
// of the configuration cascade. All fields are pointer types so Merge can
// distinguish "not set" from "set to zero value".
type DisplayConfig struct {
	ShowToolCalls         *ToolCallDisplay `toml:"show_tool_calls"         desc:"How tool-call activity appears in chat: off hides it, preview shows it then replaces it with the final reply, full keeps it visible as a separate message" choices:"off,preview,full"`                             // tool call display: off, preview, full
	ShowThinking          *ShowThinking    `toml:"show_thinking"           desc:"How the model's reasoning is shown: off hides it, compact adds a Show thinking toggle button, true prepends it to every reply" choices:"off,compact,true"`                                                         // thinking display: off, compact, true
	StreamOutput          *bool            `toml:"stream_output"           desc:"Edit the chat message in place as the reply is generated, instead of sending it only once it's complete"`                                                                                                          // stream model output in real-time
	StreamInterval        *string          `toml:"stream_interval"         desc:"How often the in-progress reply is updated on screen while streaming; lower values look smoother but send more edits to the chat platform" type:"duration"`                                                        // duration between message edits during streaming
	Streaming             *bool            `toml:"streaming"               hot:"turn" scope:"global,agent" desc:"Call the model's streaming API so tokens arrive incrementally, rather than waiting for the full response in one call; separate from stream_output, which controls whether the chat message itself is live-edited"` // use streaming API
	DisplayWidth          *int             `toml:"display_width"           desc:"Character width used for divider lines and to wrap tables and thinking blocks in chat messages"`                                                                                                                   // display width for dividers
	ReceivedFilesDir      *string          `toml:"received_files_dir"      desc:"Local directory where files received from users, such as photos or documents, are saved; leave empty to not save them"`                                                                                            // save received files to this directory
	InjectedMessageHeader *string          `toml:"injected_message_header" desc:"Text prepended to system-injected messages, such as warnings, so you can tell them apart from normal replies; empty adds no header"`                                                                               // header prepended to injected messages
	Statusline            *string          `toml:"statusline"              desc:"Template for the small header shown above each reply, such as model name and timing; leave empty to use the built-in default format"`                                                                              // per-message header template (#831)
}

// AccessConfig holds access control settings that can be set at any level
// of the configuration cascade.
type AccessConfig struct {
	AllowedUsersOnly *bool    `toml:"allowed_users_only" default:"true" desc:"When enabled (default), only user IDs in allowed_users may message this agent - an empty list blocks everyone. When disabled, an empty list allows anyone; a non-empty list still filters"`
	AllowedUsers     []string `toml:"allowed_users"`   // platform-specific user IDs allowed to interact
	RequireMention   *bool    `toml:"require_mention"` // require @mention in group chats
}

// NotifyConfig holds notification/warning settings that can be configured at
// any scope level. Resolution follows the 5-level cascade via Merge.
// All fields are nillable so nil means "not set, inherit from wider scope."
type NotifyConfig struct {
	InjectAgentWarnings *InjectionLevel `toml:"inject_agent_warnings"                   desc:"Whether internal warnings are fed directly into the agent's own conversation, as if said by the system, so it can see and react: all, errors only, or off"` // inject warnings/errors into agent session
	InjectChatWarnings  *InjectionLevel `toml:"inject_chat_warnings"                    desc:"Whether internal warnings are sent to you as chat notification messages: all, errors only, or off"`                                                         // send warnings/errors as chat notifications
	StartupNotify       *bool           `toml:"startup_notify"           default:"true" desc:"Send a chat notification each time this agent starts up"`                                                                                                   // send startup notification
	CompactionNotify    *bool           `toml:"compaction_notify"        default:"true" desc:"Send a chat notification whenever the conversation history is compacted (summarized to free up context space)"`                                             // send notification on compaction
	TaskListNotify      *bool           `toml:"task_list_notify"         default:"true" desc:"Send a chat notification whenever the agent's todo list changes"`                                                                                           // send notification on task list changes
	CompactionDebug     *bool           `toml:"compaction_debug"                        desc:"Attach the full compaction summary as a file whenever compaction runs, to inspect what got summarized"`                                                     // send compaction summary as file attachment
	WarningMaxPerWindow *int            `toml:"warning_max_per_window"   default:"3"    scope:"global,agent" desc:"Maximum identical warning notifications sent within a time window before further repeats are suppressed, to avoid spamming chat (default 3)"`               // max identical warnings per window before suppression (default 3)
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
	Bot             string   `toml:"bot"`
	BotSecret       string   `toml:"bot_secret"`
	FacetBots       []string `toml:"facet_bots"`
	FacetSessionTTL string   `toml:"facet_session_ttl"  desc:"How long a /facet session, a temporary secondary bot split off from this chat, can sit idle before its slot is reclaimed for reuse (default 60m)" type:"duration"`

	// Platform-specific subsections (at most one non-nil, must match ID)
	Telegram *TelegramSpecific `toml:"telegram"`
	Discord  *DiscordSpecific  `toml:"discord"`
	App      *AppSpecific      `toml:"app"`
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
	LongPollTimeout string  `toml:"long_poll_timeout"`                                                                                                                                                 // default "30s" (HTTP-client timeout; Telegram-side long-poll derived as -5s)
	TableWrapLines  *int    `toml:"table_wrap_lines"  desc:"Maximum number of lines a single table cell wraps to before its content is truncated (default 5)"`                                         // default 5
	TableStyle      *string `toml:"table_style"       desc:"How tables are rendered in chat: pretty uses box-drawing characters, markdown uses plain markdown table syntax" choices:"pretty,markdown"` // default "pretty"

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

// AppSpecific holds app-provider-only config (the native-app WebSocket platform,
// FAP v1). All tuning knobs are optional pointers so the agent→global→code
// cascade can resolve them; nil falls back to the code default.
type AppSpecific struct {
	Host            string `toml:"host"             desc:"Public hostname the FOCI app should use to reconnect to this server, sent to the app during the initial connection handshake"`
	Push            *bool  `toml:"push"             desc:"Send a Firebase Cloud Messaging push notification to wake the app when it's offline and a message arrives; requires the app.fcm_credentials secret"`
	ReplayBuffer    *int   `toml:"replay_buffer"    desc:"Number of recent server messages kept in memory per conversation so the app can catch up on anything missed after reconnecting (default 1000)"`
	ReplayTTL       string `toml:"replay_ttl"       desc:"Maximum age of a message kept in the in-memory reconnect-replay buffer before it's discarded (default 24h)" type:"duration"`
	ReplayStoreTTL  string `toml:"replay_store_ttl" desc:"Maximum age of a message kept in the on-disk replay database used to backfill longer gaps after reconnecting (default 30 days)" type:"duration"`
	ReplayStorePath string `toml:"replay_store_path" desc:"File path, relative to the server's data directory, of the database that durably stores replay messages (default app-frames.db)"`
	MaxBlobMB       *int   `toml:"max_blob_mb"      desc:"Maximum size, in megabytes, of a file the app can upload via the /app/blob endpoint (default 50)"`
	BlobTTL         string `toml:"blob_ttl"         desc:"How long an uploaded blob is kept on the server before it's deleted (default 24h)" type:"duration"`
	PushCoalesce    string `toml:"push_coalesce"    desc:"Minimum time between wake-up push notifications for the same conversation, to avoid a flurry of pushes when several messages arrive close together (default 15s)" type:"duration"`
	FCMCredentials  string `toml:"fcm_credentials"  desc:"Path to a Firebase Cloud Messaging service-account JSON key file, used instead of the app.fcm_credentials secret for sending wake pushes"`
	DevicesPath     string `toml:"devices_path"     desc:"File path, relative to the server's data directory, where paired app devices are stored (default app-devices.json)"`
	// AllowedDevices: if non-empty, only these device IDs may pair (empty allows
	// any). A slice → set in the TOML file, not via /config set, so no desc tag
	// (mirrors AccessConfig.AllowedUsers).
	AllowedDevices []string `toml:"allowed_devices"`
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
	if p.App == nil && defaults.App != nil {
		cp := *defaults.App
		p.App = &cp
	} else if p.App != nil && defaults.App != nil {
		if p.App.Host == "" {
			p.App.Host = defaults.App.Host
		}
		if p.App.Push == nil {
			p.App.Push = defaults.App.Push
		}
		if p.App.ReplayBuffer == nil {
			p.App.ReplayBuffer = defaults.App.ReplayBuffer
		}
		if p.App.ReplayTTL == "" {
			p.App.ReplayTTL = defaults.App.ReplayTTL
		}
		if p.App.ReplayStoreTTL == "" {
			p.App.ReplayStoreTTL = defaults.App.ReplayStoreTTL
		}
		if p.App.ReplayStorePath == "" {
			p.App.ReplayStorePath = defaults.App.ReplayStorePath
		}
		if p.App.MaxBlobMB == nil {
			p.App.MaxBlobMB = defaults.App.MaxBlobMB
		}
		if p.App.BlobTTL == "" {
			p.App.BlobTTL = defaults.App.BlobTTL
		}
		if p.App.PushCoalesce == "" {
			p.App.PushCoalesce = defaults.App.PushCoalesce
		}
		if p.App.FCMCredentials == "" {
			p.App.FCMCredentials = defaults.App.FCMCredentials
		}
		if p.App.DevicesPath == "" {
			p.App.DevicesPath = defaults.App.DevicesPath
		}
		if p.App.AllowedDevices == nil {
			p.App.AllowedDevices = defaults.App.AllowedDevices
		}
	}
}

type SessionsConfig struct {
	Dir string `toml:"dir"`

	CompactionConfig          // compaction settings (global defaults, overridable per-agent)
	CompactionMaxTokens   int `toml:"compaction_max_tokens"         default:"4096"  desc:"Max tokens the model can generate for the summary when compaction trims old conversation history to fit the context window" min:"0"` // max output tokens for summary
	CompactionMinMessages int `toml:"compaction_min_messages"       default:"4"     desc:"Minimum number of messages a session needs before compaction (automatic history trimming) is allowed to run" min:"0"`                // min messages before compacting
	MaxSystemPromptFile   int `toml:"max_system_prompt_chars_file"  default:"20000" desc:"Warn at startup if any single system prompt file (character, skill, etc) exceeds this many characters; does not block anything"`     // per-file char threshold for warnings
	MaxSystemPromptTotal  int `toml:"max_system_prompt_chars_total" default:"80000" desc:"Warn at startup if the combined size of all system prompt files exceeds this many characters; does not block anything"`              // total system prompt char threshold

	BranchOrientationFacetPrompt    *string `toml:"branch_orientation_facet_prompt"`    // path to prompt file for user-attached facet branches
	BranchOrientationHeadlessPrompt *string `toml:"branch_orientation_headless_prompt"` // path to prompt file for headless branches (cron, spawn, keepalive)

	ArchiveAfter string `toml:"archive_after" default:"24h"`  // gzip idle sessions after this duration (default "24h")
	FileMode     string `toml:"file_mode"     default:"0600"` // octal file permissions for session files (default "0600")

	EphemeralRetentionDays int `toml:"ephemeral_retention_days" default:"30" hot:"event" desc:"Days to keep ephemeral branch and fork session transcripts before automatic daily cleanup deletes them; 0 disables cleanup" min:"0"` // daily GC of stale ephemeral session files
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
	SearchBackend      *string        `toml:"search_backend"      default:"bleve" desc:"Which engine powers memory search: fts5 also searches conversation history, bleve only searches memory files but adds relevance ranking"`                   // search backend: "fts5" or "bleve"
	ReindexDebounce    *string        `toml:"reindex_debounce"    desc:"How long to wait after a memory file changes before reindexing it, so rapid edits do not trigger repeated reindexing; 0s reindexes immediately" type:"duration"`            // delay before reindex (e.g., "500ms", "2s"), default "0s"
	ConversationWeight *float64       `toml:"conversation_weight" default:"0.1"   desc:"Relevance multiplier for conversation-history hits in memory search, relative to memory-file hits; 0 excludes them, 1 weighs them equally" min:"0" max:"1"` // weight multiplier for conversation search results (default 0.1)
	SearchLimit        *int           `toml:"search_limit"        default:"20"    desc:"Maximum number of results the memory search tool returns for a single query"`                                                                               // max search results to return (default 20)
	SweepInterval      *string        `toml:"sweep_interval"      default:"0"     desc:"How often to fully rebuild the memory search index from scratch; 0 disables periodic rebuilds since file-watching already catches changes" type:"duration"` // periodic full reindex interval (default "0"=disabled; fsnotify watch already catches file changes). Set e.g. "1h" to re-enable.
	// Temporal decay (#352, bleve backend): boost recent results in relevance
	// search. Recency-boost only — old results are never penalised.
	TemporalDecay *bool    `toml:"temporal_decay"     default:"true" desc:"Boost more recently modified memory files higher in search relevance ranking; applies to both search backends"`
	DecayHalfLife *float64 `toml:"decay_half_life"    default:"10"   desc:"Number of days after which the recency boost from temporal_decay has halved in strength" min:"0"`
	DecayBoost    *float64 `toml:"decay_boost"        default:"1"    desc:"Strength of the recency boost from temporal_decay: a brand-new file's score is multiplied by up to 1+this value" min:"0"`
	// Basename globs never recency-boosted (default MEMORY.md, research-*). A slice
	// field, so no desc tag (the /config-set registry only handles scalars).
	EvergreenPatterns []string `toml:"evergreen_patterns"`
}

type HTTPConfig struct {
	Port                    int    `toml:"port" default:"18791" desc:"TCP port the foci HTTP server listens on" min:"1" max:"65535"`
	Bind                    string `toml:"bind" default:"127.0.0.1" desc:"Network address the HTTP server binds to; 127.0.0.1 only accepts local connections, 0.0.0.0 accepts connections from other machines"`
	GracefulShutdownTimeout string `toml:"graceful_shutdown_timeout" default:"30s"` // time to wait for in-flight requests on shutdown (default "30s")
	WSEnabled               bool   `toml:"ws_enabled"`                              // enable /voice WebSocket endpoint (default false)
	SocketPath              string `toml:"socket_path"`                             // Unix socket path for same-user auth (default: auto-resolved to data dir)
}

type LoggingConfig struct {
	Level           string `toml:"level"      default:"INFO" hot:"immediate" desc:"Minimum severity written to the log file: DEBUG is most verbose, ERROR is least; each level also includes all levels above it" choices:"DEBUG,INFO,WARN,ERROR"`
	EventFile       string `toml:"event_file" default:"logs/foci.log"`
	APIFile         string `toml:"api_file"   default:"logs/api.jsonl"`
	APIDB           string `toml:"api_db" default:"api.db"` // SQLite API call log path (relative to data_dir)
	ConversationLog *bool  `toml:"conversation_log" default:"true" desc:"Log each agent's conversation turns to disk; turn this off to avoid persisting conversation content"`

	FullPayload bool   `toml:"full_payload"                         desc:"Write the complete raw request and response sent to the model API to logs/api-payload.jsonl, useful for debugging"` // write full API payloads to api-payload.jsonl
	PayloadFile string `toml:"payload_file"             default:"logs/api-payload.jsonl"`                                                                                                     // path for full API payload log

	WarningWindowDuration             string `toml:"warning_window_duration"              default:"5m"  desc:"Time window used to group and rate-limit repeated warnings so identical ones are not injected into the agent repeatedly" type:"duration"` // time window for warning dedup (default "5m")
	WarningProactiveActiveInterval    string `toml:"warning_proactive_active_interval"    default:"5m"  desc:"Minimum time between unprompted warning notifications sent while you have been recently active" type:"duration"`                          // min interval between proactive warning turns when user is active (default "5m")
	WarningProactiveInactiveInterval  string `toml:"warning_proactive_inactive_interval"  default:"1h"  desc:"Minimum time between unprompted warning notifications sent while you have not been recently active" type:"duration"`                      // min interval when user is inactive (default "1h")
	WarningProactiveActivityThreshold string `toml:"warning_proactive_activity_threshold" default:"10m" desc:"How recently you must have sent a message to count as active for the proactive warning intervals above" type:"duration"`                  // user is "active" if last message within this window (default "10m")

	LogRotation         *bool  `toml:"log_rotation"            default:"true" desc:"Automatically rotate and gzip-archive log files instead of letting them grow forever"`                                                        // enable built-in log rotation (default true)
	RotationPeriod      string `toml:"rotation_period"        default:"24h"   desc:"How often the log rotation check runs, archiving old log content" type:"duration"`                                                            // how often to rotate (default "24h")
	RetentionPeriod     string `toml:"retention_period"       default:"48h"   desc:"How long log lines stay in the live log file before being archived; older lines are moved into gzip files under archive_dir" type:"duration"` // keep lines newer than this (default "48h")
	RotationMaxLineSize string `toml:"rotation_max_line_size" default:"64MB"  desc:"Largest single log line the rotator can read; a longer line is skipped instead of crashing the rotation process"`                             // max line size for scanner buffer (default "64MB")
	ArchiveDir          string `toml:"archive_dir"`                                                                                                                                                                               // gzip archive directory (default: log_dir/archive/)
	LogFileMode         string `toml:"log_file_mode"          default:"0600"  desc:"Unix file permissions (octal, e.g. 0600) applied to log files"`                                                                               // octal file permissions for log files (default "0600")
}

// TTSConfig describes a text-to-speech provider entry.
// Multiple entries are supported via [[tts]]; first entry is the default.
type TTSConfig struct {
	ID             string            `toml:"id"`              // lookup key for agent overrides
	Format         string            `toml:"format"`          // "openai" or "edge-tts"
	Endpoint       string            `toml:"endpoint"`        // API URL (ignored for edge-tts)
	Model          string            `toml:"model"`           // model name (ignored for edge-tts)
	Voice          string            `toml:"voice"`           // voice name (format-specific)
	Rate           float64           `toml:"rate"`            // speed multiplier: 1.0 = normal, 0 = omit
	Secret         string            `toml:"secret"`          // secret name in secrets.toml (optional, fallback: hostname)
	Command        string            `toml:"command"`         // binary for edge-tts (default: "edge-tts")
	ResponseFormat string            `toml:"response_format"` // audio format: "mp3", "wav", etc. (default: "wav")
	Replacements   map[string]string `toml:"replacements"`    // word replacements applied before synthesis (e.g. "foci" = "foki")
}

// STTConfig describes a speech-to-text provider entry.
// Multiple entries are supported via [[stt]]; first entry is the default.
type STTConfig struct {
	ID           string            `toml:"id"`           // lookup key for agent overrides
	Format       string            `toml:"format"`       // "openai" (only supported format currently)
	Endpoint     string            `toml:"endpoint"`     // API URL
	Model        string            `toml:"model"`        // model name
	Secret       string            `toml:"secret"`       // secret name in secrets.toml (optional, fallback: hostname)
	Replacements map[string]string `toml:"replacements"` // word replacements applied after transcription (e.g. "foki" = "foci")
}

type BitwardenConfig struct {
	Enabled         bool   `toml:"enabled"`
	RefreshInterval string `toml:"refresh_interval" default:"15m"`                         // how often to refresh item metadata (default "15m")
	SecretTTL       string `toml:"secret_ttl"       default:"30m"`                         // how long unlocked values stay cached (default "30m")
	SessionFile     string `toml:"session_file"     default:"/home/bitwarden/.bw_session"` // path to BW session file read by bitwarden user (default "/home/bitwarden/.bw_session")
	CleanupInterval string `toml:"cleanup_interval" default:"1m"`                          // how often to purge expired values (default "1m")
}

// PermissionsConfig controls foci-level auto-approval of delegated backend
// permission requests. Rules from global [permissions] and per-agent
// [[agents]].permissions are combined (union) — both sets apply.
// All fields are pointer/slice types for Merge-based resolution.
type PermissionsConfig struct {
	AutoApprove                []string `toml:"auto_approve"`                                                                                                                                                                                          // glob patterns (e.g. "Bash:git *") to auto-approve without prompting
	AutoApproveCommonReadonly  *bool    `toml:"auto_approve_common_readonly"  default:"true"  desc:"Automatically approve a built-in list of safe, read-only tools and shell commands (ls, cat, git status, etc) without prompting"`                   // enable built-in read-only tool/command allowlist
	AutoApproveCommonSafeWrite *bool    `toml:"auto_approve_common_safe_write" default:"false" desc:"Automatically approve a built-in list of commonly-safe but side-effecting commands like curl, wget, mkdir and touch; not restricted to any path"` // enable built-in safe-write allowlist (default false — not path-scoped)
	// PromptTTL is how long an unanswered interactive prompt (permission
	// request, AskUserQuestion) stays live before it auto-expires. On expiry
	// the prompt is resolved as a denial/cancel so the waiting backend doesn't
	// orphan, and its message is edited to show it expired. The effective TTL
	// is capped at the delegated-backend idle timeout — min(prompt_ttl,
	// idle_timeout) — since a prompt can't outlive the backend that's waiting
	// on it (the idle reaper clears it). Parsed via time.ParseDuration.
	PromptTTL string `toml:"prompt_ttl" default:"24h" scope:"global" desc:"How long a permission request or question can sit unanswered before it is automatically denied so the agent is not stuck waiting"`
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
	Enabled  *bool   `toml:"enabled"    default:"true"         desc:"Include an Environment section in the agent's system prompt describing the platform, active tools and docs it has access to"`                // inject environment block as first system block (default true)
	DocsPath *string `toml:"docs_path"  default:"shared/docs"  desc:"Directory of platform documentation the agent can reference, listed in the Environment block; relative paths resolve against your home dir"` // path to platform docs directory; relative paths resolve against $HOME
}

type SkillsConfig struct {
	Dir string `toml:"dir"` // shared skills directory (default: $home/shared/skills/)
}

type ResourcesConfig struct {
	MemoryGuardEnabled        *bool    `toml:"memory_guard_enabled"        default:"true" desc:"Watch system RAM use and warn or kill runaway processes when memory runs critically low"`                                                          // enable system memory guard (default true)
	MemoryGuardInterval       string   `toml:"memory_guard_interval"       default:"60s"  desc:"How often the memory guard checks RAM usage, as a duration like 60s" type:"duration"`                                                              // check interval (default "60s")
	MemoryWarnPercent         *int     `toml:"memory_warn_percent"         default:"25"   desc:"RAM usage, as a percent of total system memory, above which the memory guard starts warning (if memory pressure is also high)"`                    // warn threshold as % of total RAM (default 25)
	MemoryKillPercent         *int     `toml:"memory_kill_percent"         default:"40"   desc:"RAM usage, as a percent of total system memory, above which the memory guard kills the largest runaway process (if memory pressure is also high)"` // kill threshold as % of total RAM (default 40)
	MemoryPressureThreshold   *float64 `toml:"memory_pressure_threshold"   default:"10"   desc:"Linux memory-pressure value (PSI avg10) that must also be reached before the warn or kill percent thresholds take effect"`                         // PSI avg10 threshold to require before acting (default 10.0)
	GoroutineMonitorInterval  string   `toml:"goroutine_monitor_interval"  default:"60s"  desc:"How often to check the running goroutine count for possible leaks, as a duration like 60s" type:"duration"`                                        // goroutine count check interval (default "60s")
	GoroutineMonitorThreshold int      `toml:"goroutine_monitor_threshold"                desc:"Log a warning if the goroutine count exceeds this. 0 auto-calculates a threshold from the number of agents and telegram bots configured"`          // warn when goroutine count exceeds this (0 = auto: 30 + 25×agents + 5×telegram_bots)
}

// BrowserConfig holds configuration for the browser automation tool.
// All fields are pointer types for Merge-based resolution (per-agent → global).
// TOML: [browser] globally, [[agents]].browser per-agent.
type BrowserConfig struct {
	Enabled        *bool    `toml:"enabled"         default:"true" desc:"Enable the browser automation tool, letting the agent open and control a Chrome browser"`                 // enable browser tool
	Headless       *bool    `toml:"headless"        default:"true" desc:"Run the automated Chrome browser with no visible window"`                                                 // run headless
	TimeoutSec     *int     `toml:"timeout_sec"     default:"30"   desc:"Seconds to wait for a browser page operation, like a click or navigation, before it times out"`           // page operation timeout in seconds
	UserDataDir    *string  `toml:"user_data_dir"                  desc:"Directory Chrome stores its browser profile in. Leave empty to use a fresh temporary profile each time"`  // Chrome user data dir (empty = temp profile)
	ExecutablePath *string  `toml:"executable_path"                desc:"Path to the Chrome binary to launch. Leave empty to auto-detect an installed Chrome"`                     // Chrome executable path (empty = auto-detect)
	DOMStableSec   *float64 `toml:"dom_stable_sec"  default:"1"    desc:"Seconds between page snapshots when waiting for a webpage to stop changing before treating it as loaded"` // DOM stability wait interval in seconds
	DOMStableDiff  *float64 `toml:"dom_stable_diff" default:"0.2"  desc:"How much a page may differ between snapshots, as a fraction from 0 to 1, and still count as stable"`      // DOM stability diff threshold
}

type ToolsConfig struct {
	SummaryConfig // global summary/tool-result defaults (resolved via Merge with per-agent)
	ToolConfig    // global tool behavioral defaults (resolved via Merge with per-agent)

	TempDir                 string   `toml:"temp_dir"                   default:"/tmp/foci/tool-results"`                                                                                // where to write large tool results (default /tmp/foci/tool-results)
	TmuxCols                int      `toml:"tmux_cols"                  default:"300"       desc:"Number of columns for the tmux terminal window created for running shell commands"`    // tmux window columns on start (default 300)
	TmuxRows                int      `toml:"tmux_rows"                  default:"30"        desc:"Number of rows for the tmux terminal window created for running shell commands"`       // tmux window rows on start (default 30)
	ExecDefaultTimeout      int      `toml:"exec_default_timeout"       default:"30"        desc:"Default number of seconds a shell command may run before it is timed out"`             // default timeout for exec commands in seconds (default 30)
	TmuxCommandTimeout      string   `toml:"tmux_command_timeout"       default:"5s"`                                                                                                    // timeout for tmux control commands (default "5s")
	WebFetchMaxBytes        int      `toml:"web_fetch_max_bytes"        default:"1048576"`                                                                                               // max bytes to read from web fetch (default 1048576 = 1MB)
	ToolCallPreviewChars    int      `toml:"tool_call_preview_chars"    default:"450"       desc:"Maximum characters of a tool call's parameters shown in the Telegram preview message"` // max chars for tool call param preview in Telegram (default 450)
	TmuxMemoryCheckInterval string   `toml:"tmux_memory_check_interval" default:"5m"`                                                                                                    // how often to check tmux RSS (default "5m", "0" disables)
	TmuxMemoryWarn          string   `toml:"tmux_memory_warn"           default:"10%"`                                                                                                   // warn threshold as % of RAM or absolute (default "10%")
	TmuxMemoryCritical      string   `toml:"tmux_memory_critical"       default:"20%"`                                                                                                   // critical threshold (default "20%")
	TmuxMemoryKill          string   `toml:"tmux_memory_kill"           default:"30%"`                                                                                                   // kill threshold (default "30%")
	WebSearchMaxUses        int      `toml:"web_search_max_uses"`                                                                                                                        // max searches per API call (0 = unlimited)
	WebSearchAllowedDomains []string `toml:"web_search_allowed_domains"`                                                                                                                 // domain whitelist (mutually exclusive with blocked)
	WebSearchBlockedDomains []string `toml:"web_search_blocked_domains"`                                                                                                                 // domain blacklist
	WebFetchMaxUses         int      `toml:"web_fetch_max_uses"`                                                                                                                         // max fetches per API call (0 = unlimited)
	WebFetchAllowedDomains  []string `toml:"web_fetch_allowed_domains"`                                                                                                                  // domain whitelist
	WebFetchBlockedDomains  []string `toml:"web_fetch_blocked_domains"`                                                                                                                  // domain blacklist
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
	Enabled          *bool   `toml:"enabled"                  hot:"event" desc:"Enable the keepalive timer, which sends periodic no-op prompts to keep the model provider's prompt cache warm"` // enable keepalive timer
	Interval         *string `toml:"interval" default:"55m"   hot:"event" desc:"How long since the prompt cache was last warmed before the keepalive timer fires again" type:"duration"`        // time since cache last warmed before firing
	Prompt           *string `toml:"prompt"                   hot:"event" desc:"Path to a custom keepalive prompt file. Leave unset for the built-in default, or set to none to disable it"`    // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
	WarmOpenAppChats *bool   `toml:"warm_open_app_chats"      desc:"When true, keepalive warms every chat currently open in the app instead of just the main session, so their caches stay warm too"`
}

// ReflectionConfig controls the periodic reflection pass, which captures both
// factual memory (memory files) and procedural knowledge (autogenerated skills)
// from recent session activity. All fields are pointer types for Merge-based
// resolution (per-agent → global).
type ReflectionConfig struct {
	IntervalEnabled       *bool   `toml:"interval_enabled"       default:"true" hot:"event" desc:"Run a reflection pass on a timer to capture memories and skills from recent activity, alongside any other reflection triggers"`                    // periodic reflection on timer
	Interval              *string `toml:"interval"               default:"1h"   hot:"event" desc:"How long to wait between timer-triggered reflection passes" type:"duration"`                                                                       // time between reflections
	IntervalPrompt        *string `toml:"interval_prompt"                       hot:"event" desc:"Path to a custom prompt for timer-triggered reflection. Leave unset for the built-in default, or set to none to disable this trigger"`             // prompt override (nil = embedded, "none" = disabled)
	SessionEndEnabled     *bool   `toml:"session_end_enabled"    default:"true" desc:"Run a reflection pass whenever a session is reset or reclaimed, to capture memories and skills before it ends"`                                    // reflect on /reset and reclaim
	SessionEndPrompt      *string `toml:"session_end_prompt"                    desc:"Path to a custom prompt for session-end reflection. Leave unset for the built-in default, or set to none to disable this trigger"`                 // prompt override (nil = embedded, "none" = disabled)
	CompactionEnabled     *bool   `toml:"compaction_enabled"     default:"true" desc:"Run a reflection pass right before a session is compacted, so memories and skills are captured before older messages are summarized away"`         // reflect before compaction
	CompactionPrompt      *string `toml:"compaction_prompt"                     desc:"Path to a custom prompt for pre-compaction reflection. Leave unset for the built-in default, or set to none to disable this trigger"`              // prompt override (nil = embedded, "none" = disabled)
	BackendQuietPeriod    *string `toml:"backend_quiet_period"        default:"5m"   hot:"event" desc:"For delegated backends like Claude Code, how long the user must be idle before reflection is injected into the live session" type:"duration"` // min idle before firing in backend mode
	NotifyOnSkillCreation *bool   `toml:"notify_on_skill_creation"     default:"true" hot:"event" desc:"Send a chat message telling the user when a reflection pass creates or updates a skill"`                                                     // notify user on skill creation/update during reflection
}

// SchedulerConfig controls the periodic scheduler that drives all four timers
// (keepalive, background, reflection, consolidation) off a single ticker.
// tick_interval is purely the poll cadence — the real thresholds live in each
// timer's own interval — so lowering it only makes the timers respond sooner.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type SchedulerConfig struct {
	TickInterval *string `toml:"tick_interval" default:"30s" hot:"event" desc:"How often the scheduler checks whether keepalive, background, reflection or consolidation are due. Only affects polling latency, not how often they fire" type:"duration"` // how often the periodic timers are checked
}

// MaintenanceConfig controls scheduled housekeeping that runs at a wall-clock
// time of day or on a fixed interval: MEMORY.md consolidation and daily session
// reset. Both consolidation_time and reset_time accept EITHER a "HH:MM" clock
// time (interpreted in the process timezone, daily) OR a Go duration like "20h"
// (fixed interval since the last run). All fields are pointer types for
// Merge-based resolution (per-agent → global).
type MaintenanceConfig struct {
	ConsolidationEnabled *bool   `toml:"consolidation_enabled" default:"true" hot:"event" desc:"Periodically curate the recent daily memory files into the long-term MEMORY.md file"`                                                                  // curate MEMORY.md periodically
	ConsolidationTime    *string `toml:"consolidation_time"    default:"20h"  hot:"event" desc:"When to run MEMORY.md consolidation: either a daily clock time like 20:00, or a duration like 20h since the last run"`                                 // "HH:MM" daily or duration
	ConsolidationPrompt  *string `toml:"consolidation_prompt"                 hot:"event" desc:"Path to a custom consolidation prompt file. Leave unset for the built-in default, or set to none to disable consolidation"`                            // prompt override (nil = embedded, "none" = disabled)
	ConsolidationMaxIdle *string `toml:"consolidation_max_idle" default:"1h" hot:"event" desc:"Skip consolidation if the user has not interacted within this window, since there is nothing new to curate" type:"duration"`                            // skip if idle longer than this
	ResetTime            *string `toml:"reset_time"            default:""     hot:"event" desc:"When to reset the daily session: a clock time like 04:00, a duration like 24h since the last reset, or empty to never auto-reset"`                     // "HH:MM" daily, duration, or "" = never
	ResetIdleGuard       *string `toml:"reset_idle_guard"      default:"55m"  hot:"event" desc:"Skip a scheduled session reset if the user interacted within this window, so an active conversation is not wiped out from under them" type:"duration"` // skip reset if recently active
}

// BackgroundConfig controls the background work timer.
// All fields are pointer types for Merge-based resolution (per-agent → global).
type BackgroundConfig struct {
	Enabled          *bool   `toml:"enabled"                  hot:"event" desc:"Enable the background work timer, which lets the agent do unprompted work between user interactions"`                                    // enable background work timer
	Interval         *string `toml:"interval" default:"15m"   hot:"event" desc:"How long since the last user interaction before the background work timer fires" type:"duration"`                                        // time since last interaction before firing
	Prompt           *string `toml:"prompt"                   hot:"event" desc:"Path to a custom background work prompt file. Leave unset for the built-in default, or set to none to disable background work"`          // prompt file path (nil = embedded default, "none" = disabled, "default" = embedded)
	CanRunBackground *string `toml:"can_run_background"       hot:"event" desc:"Script run to gate background work, reflection and consolidation. Exit code 0 allows it, non-zero skips it. Empty means always allowed"` // path to a script/executable run before each background op; exit 0 permits the work, any non-zero exit skips it. Empty = always allowed.
}

// DebugConfig holds developer/debugging knobs that can be configured at
// any scope level. Resolution follows the 5-level cascade via Merge.
// All fields are nillable so nil means "not set, inherit from wider scope."
type DebugConfig struct {
	LogAPIKeySuffix      *bool `toml:"log_api_key_suffix"      scope:"global" desc:"Log the last 4 characters of the API key on every model provider request, useful for confirming which key was used"`                                   // log last 4 chars of API keys on each provider call (default false)
	MessagesInLog        *bool `toml:"messages_in_log"         scope:"global,agent" desc:"Include the full text of user messages in the event log. Off by default to avoid writing personal conversation content to logs"`                       // log user message content to event log (default false for privacy)
	CacheBustDetect      *bool `toml:"cache_bust_detect"       default:"false" hot:"turn" scope:"global,agent" desc:"Alert in chat when the provider's prompt-cache hit count drops between requests, which can signal the cache was unexpectedly evicted"` // alert when cache_read drops >50% vs previous request
	CacheBustIdleMinutes *int  `toml:"cache_bust_idle_minutes" default:"10"    hot:"turn" scope:"global,agent" desc:"Suppress the cache-bust alert if the session has been idle longer than this many minutes, since the cache naturally expires by then"`  // suppress cache bust alert if session idle > N minutes (default 10)

	// EnablePprof exposes the net/http/pprof endpoints under /debug/pprof/*.
	// Off by default: they allow CPU/heap profiling and goroutine dumps, so they
	// are gated behind an explicit opt-in even though the HTTP server is
	// auth-gated. Process-global (top-level [debug] section).
	EnablePprof *bool `toml:"enable_pprof" default:"false" hot:"immediate" scope:"global" desc:"Expose Go profiling endpoints at /debug/pprof/* for CPU, memory and goroutine diagnostics. Off by default since profiling data can be sensitive"`

	// Per-package "extra" verbose logging. Each switches on investigation-grade
	// logging for one package, tagged "xtra:<package>" in the log (grep
	// "xtra:ccstream", or "xtra:" for all). Process-global (applied once at
	// startup from the top-level [debug] section); default off. See log.Extra.
	ExtraCcstreamLogging *bool `toml:"extra_ccstream_logging" default:"false" hot:"immediate" scope:"global" desc:"Log verbose details (tagged xtra:ccstream) of the Claude Code backend streaming transport, for debugging delegated Claude Code sessions"` // verbose ccstream turn/steer logging
	ExtraTelegramLogging *bool `toml:"extra_telegram_logging" default:"false" hot:"immediate" scope:"global" desc:"Log verbose details (tagged xtra:telegram) of the Telegram bot's polling and message transport, for debugging Telegram connectivity"`     // verbose telegram poll/transport logging
	ExtraInboxLogging    *bool `toml:"extra_inbox_logging"    default:"false" hot:"immediate" scope:"global" desc:"Log verbose details (tagged xtra:inbox) of how incoming messages are queued, steered or dropped, for debugging message routing"`          // verbose inbox routing/gate logging
}

type Config struct {
	DataDir            string                    `toml:"data_dir"`   // directory for databases, sessions, state (default: $HOME/data)
	Notify             NotifyConfig              `toml:"notify"`     // global notification defaults
	Display            DisplayConfig             `toml:"display"`    // global display defaults
	Nudge              NudgeConfig               `toml:"nudge"`      // global nudge defaults
	Voice              VoiceConfig               `toml:"voice"`      // global voice defaults
	AgentLoop          AgentLoopConfig           `toml:"agent_loop"` // global agent loop defaults
	Behavior           BehaviorConfig            `toml:"behavior"`   // global behavior defaults
	System             SystemConfig              `toml:"system"`     // global system defaults
	Groups             GroupsConfig              `toml:"groups"`     // model group assignments and fallbacks
	Models             map[string]ModelConfig    `toml:"models"`     // named model definitions with per-model settings
	Endpoints          map[string]EndpointConfig `toml:"endpoints"`  // named API endpoints (built-in: anthropic, gemini, openai, openrouter)
	Agents             []AgentConfig             `toml:"agents"`     // multi-agent: array of agents
	Anthropic          AnthropicConfig           `toml:"anthropic"`
	Platforms          []PlatformConfig          `toml:"platforms"`
	Sessions           SessionsConfig            `toml:"sessions"`
	Memory             MemoryConfig              `toml:"memory"`
	HTTP               HTTPConfig                `toml:"http"`
	Logging            LoggingConfig             `toml:"logging"`
	TTS                []TTSConfig               `toml:"tts"`
	STT                []STTConfig               `toml:"stt"`
	Bitwarden          BitwardenConfig           `toml:"bitwarden"`
	Environment        EnvironmentConfig         `toml:"environment"`
	Skills             SkillsConfig              `toml:"skills"`
	Resources          ResourcesConfig           `toml:"resources"`
	Debug              DebugConfig               `toml:"debug"`
	Tools              ToolsConfig               `toml:"tools"`
	Browser            BrowserConfig             `toml:"browser"`
	Keepalive          KeepaliveConfig           `toml:"keepalive"`
	Background         BackgroundConfig          `toml:"background"`
	Reflection         ReflectionConfig          `toml:"reflection"`
	Scheduler          SchedulerConfig           `toml:"scheduler"`
	Maintenance        MaintenanceConfig         `toml:"maintenance"`
	Permissions        PermissionsConfig         `toml:"permissions"`
	CCBackend          CCBackendConfig           `toml:"cc_backend"`       // shared defaults for Claude Code delegator backends
	OpencodeBackend    OpencodeBackendConfig     `toml:"opencode_backend"` // shared defaults for opencode delegator backend
	Askgw              AskgwConfig               `toml:"askgw"`            // ask-gateway: local socket for external Apps to ask humans questions
	Commands           []CommandConfig           `toml:"commands"`
	MessageTransforms  []MessageTransform        `toml:"message_transforms"`                             // regex find/replace rules applied to inbound messages
	BlockedPaths       []BlockedPath             `toml:"blocked_paths"`                                  // path prefixes that write/edit tools refuse (with rebuke message)
	FileMode           string                    `toml:"file_mode"            default:"0640"`            // octal file permissions for workspace/content files (default "0640")
	WelcomeFile        string                    `toml:"welcome_file"         default:"data/WELCOME.md"` // path to welcome/changelog file injected on startup (e.g. /home/foci/WELCOME.md)
	Timezone           string                    `toml:"timezone"`                                       // IANA timezone for timestamps (e.g. "Europe/Athens", "UTC", "Local"); empty = machine local
	MasterAgent        string                    `toml:"master_agent"`                                   // agent that receives system injections not addressed to a specific agent (restart notices, update changelogs); empty = legacy behavior (restart → every agent, changelog → first agent)
	DefaultPlatform    string                    `toml:"default_platform"`                               // platform preferred when resolving an agent's default session and delivery fallbacks (e.g. "telegram"); per-agent default_platform overrides; empty = most-recently-active platform
	SkipSecurityChecks bool                      `toml:"skip_security_checks"`                           // if true, skip startup security checks for secrets.toml
	ShellEnvFile       *string                   `toml:"shell_env_file"`                                 // rc/env file sourced at startup so tool shells inherit the operator's common env; nil = ladder (~/.bashrc → ~/.zshenv → ~/.profile, first present); "" = load nothing; explicit path = that file. backend_config.env overrides on collision.
	DefinedKeys        map[string]bool           `toml:"-"`                                              // keys explicitly set in TOML file (populated by Load)
	SourcePath         string                    `toml:"-"`                                              // absolute-ish path the config was loaded from (populated by Load; used by config editing)
	UndefinedKeys      []string                  `toml:"-"`                                              // unrecognised TOML keys (populated by Load, logged by caller)
}

// DefaultPlatformFor resolves the preferred platform for an agent: the
// per-agent default_platform when set, else the global one. Empty means no
// preference (most-recently-active platform wins).
func (cfg *Config) DefaultPlatformFor(agentID string) string {
	for _, a := range cfg.Agents {
		if a.ID == agentID && a.DefaultPlatform != "" {
			return a.DefaultPlatform
		}
	}
	return cfg.DefaultPlatform
}
