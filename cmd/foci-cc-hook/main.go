// Command foci-cc-hook is a tiny helper that foci installs as a
// PostToolUse, PostToolUseFailure and PermissionDenied hook on Claude Code
// sessions, and as a PreToolUse hook for the Agent tool plus every tool a
// pretool rule names.
// CC invokes the configured hook binary after each tool execution,
// pipes a JSON envelope containing the tool call + its response (or
// error) into the binary's stdin, and captures the binary's stdout
// into a system/hook_response message on its stream-json output.
//
// This binary reads that JSON envelope, extracts the fields foci
// needs for OnToolEnd correlation (hook_event_name, tool_use_id,
// tool_name, tool_response, error, agent_id), truncates large
// response payloads to keep stream-json lines under ccstream's
// scanner limit, and writes a compact JSON object to stdout.
//
// On PreToolUse it also evaluates the pretool rules foci passed via --rules
// (internal/delegator/pretool). A matching deny rule adds a
// hookSpecificOutput.permissionDecision="deny" to the same JSON object; CC
// honours it and returns permissionDecisionReason to the model as the tool's
// error result, ignoring the object's other fields (verified live, CC
// 2.1.280). It never emits "allow" — see the pretool package doc. A rule's
// when-check runs here as a bash child; one that fails (timeout, error)
// never denies, and is reported in when_errors for foci to log.
//
// When an agent has stop rules (internal/delegator/stoprule, #2089) foci also
// installs it as a Stop hook, with the rules in --stop-rules. On Stop it
// evaluates the turn's final text and transcript and writes a stopOutput:
// decision="block" plus a reason when a rule fires (CC then continues the
// turn with the reason as a "Stop hook feedback" user message, verified live
// on CC 2.1.280), and in every case the verdict fields foci logs.
//
// The helper always exits 0 regardless of parse errors — CC uses
// exit codes to gate tool execution (exit 2 blocks), so we must not
// accidentally interfere with the user's turn. Any parse failure on
// our side is a silent drop; foci's stream parser will log at debug
// when it sees the empty or malformed hook_response.stdout and
// graceful-skip the OnToolEnd dispatch.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"foci/internal/delegator/pretool"
	"foci/internal/delegator/stoprule"
)

// installIDFlag is the argv flag foci sets when installing the hook so the
// binary can echo the install ID back in its output. Each foci backend uses
// a unique install ID in its hook command string — see
// internal/delegator/ccstream/hooks.go for the generation and filter logic.
// Without the ID round-trip, multiple backends sharing a workdir can't tell
// which hook_response events belong to which backend.
const installIDFlag = "--install"

// rulesFlag carries the encoded pretool rules (pretool.Encode). Only the
// PreToolUse command line has it; must match internal/delegator/ccstream.
const rulesFlag = "--rules"

// stopRulesFlag carries the encoded stop rules (stoprule.Encode). Only the
// Stop command line has it; must match internal/delegator/ccstream.
const stopRulesFlag = "--stop-rules"

// eventStop is CC's hook_event_name for the end-of-turn hook.
const eventStop = "Stop"

// eventPermissionDenied is CC's hook_event_name for an auto-mode classifier
// denial (#2184). It is the denied call's only hook: no PostToolUse(Failure)
// follows it.
const eventPermissionDenied = "PermissionDenied"

// slowToolThreshold is the runtime from which a tool call's duration is shown
// to the model (#2125, Factory's Droid idea): knowing a call was slow lets it
// avoid repeating it, pick a faster route, or set a timeout. Faster calls are
// left unannotated to keep the noise out. 10s is Dick's ruling of 2026-10-02.
const slowToolThreshold = 10 * time.Second

// maxFieldBytes bounds the size of tool_response / tool_input / error fields
// in the emitted JSON. Two independent constraints, the tighter of which sets
// the value:
//
//  1. Each hook_response line from CC must stay under the ccstream reader's
//     1MB scanner limit (internal/delegator/ccstream/reader.go maxTokenSize) —
//     without a cap, a multi-MB file read would blow the scanner and tear down
//     the backend via OnReaderStopped.
//  2. Foci's only consumer of tool_response is the tool-call display: the
//     result hint (one line) and the "Show full" expansion, which is itself
//     hard-capped at 4096 bytes for the whole message (telegram/discord
//     formatToolCallWithResult, maxLen=4096). tool_input feeds nudge matching
//     and Agent-description extraction, both of which read small fields.
//
// So nothing foci renders or matches on needs more than ~4KB; the old 64KB was
// a scanner-safety margin, not a consumer requirement. 4KB satisfies both
// constraints and slashes the dominant contributor to the CC session JSONL
// (the hook_success attachments — ~40% of the file). See docs/WIRING.md
// "Hook output path".
const maxFieldBytes = 4 * 1024

// hookInput mirrors the JSON envelope CC writes to the hook's stdin for
// PostToolUse / PostToolUseFailure events. See claude-code
// src/entrypoints/sdk/coreSchemas.ts:436-459 for the canonical schema.
// Fields not consumed by foci are intentionally omitted.
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse  json.RawMessage `json:"tool_response,omitempty"`
	Error         string          `json:"error,omitempty"`
	AgentID       string          `json:"agent_id,omitempty"`
	IsInterrupt   bool            `json:"is_interrupt,omitempty"`
	IsTimeout     bool            `json:"is_timeout,omitempty"`
	// DurationMS is the tool call's runtime, sent on PostToolUse and
	// PostToolUseFailure (verified in the CC 2.1.286 bundle); 0 when absent.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Cwd is the session's working directory; pretool rules can match it.
	Cwd string `json:"cwd,omitempty"`
	// Reason is PermissionDenied's only extra field: why CC's auto-mode
	// classifier refused the call (#2184; CC 2.1.289 bundle).
	Reason string `json:"reason,omitempty"`

	// Stop payload fields (shape verified live, CC 2.1.280).
	TranscriptPath       string `json:"transcript_path,omitempty"`
	PromptID             string `json:"prompt_id,omitempty"`
	StopHookActive       bool   `json:"stop_hook_active,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
}

// hookOutput is the compact JSON foci's ccstream handleHookResponse parser
// expects to find in hook_response.stdout. Keep field names aligned with
// the stable contract in internal/delegator/ccstream/hooks.go.
//
// InstallID is echoed back from argv so foci can filter hook_response
// events by the originating backend when multiple foci backends share a
// workdir and therefore share a .claude/settings.local.json file.
type hookOutput struct {
	HookEvent    string `json:"hook_event"`
	InstallID    string `json:"install_id,omitempty"`
	ToolUseID    string `json:"tool_use_id"`
	ToolName     string `json:"tool_name"`
	ToolInput    string `json:"tool_input,omitempty"`
	ToolResponse string `json:"tool_response,omitempty"`
	Error        string `json:"error,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	IsError      bool   `json:"is_error"`
	// DeniedRule names the pretool rule that refused this call, so foci can
	// log the deny. Empty when no rule matched.
	DeniedRule string `json:"denied_rule,omitempty"`
	// WhenErrors reports pretool when-checks that failed open (#2034), so
	// foci can log them.
	WhenErrors []string `json:"when_errors,omitempty"`
	// DurationMS echoes CC's duration_ms, and RuntimeShown reports that the
	// model was told it (#2125), so foci can log each runtime note.
	DurationMS   int64 `json:"duration_ms,omitempty"`
	RuntimeShown bool  `json:"runtime_shown,omitempty"`
	// HookSpecificOutput is the part CC acts on: a PreToolUse deny, or a
	// PostToolUse(Failure) runtime note.
	HookSpecificOutput *hookSpecific `json:"hookSpecificOutput,omitempty"`
}

// hookSpecific is CC's hookSpecificOutput shape. PreToolUse reads the
// permission fields; PostToolUse and PostToolUseFailure read
// additionalContext, which CC adds to the model's context after the tool
// result.
type hookSpecific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// stopOutput is what the helper writes on Stop. Decision and Reason are the
// part CC acts on (a block continues the turn with Reason shown to the
// agent); the rest is the verdict foci's handleHookResponse logs. Keep the
// field names aligned with internal/delegator/ccstream.
type stopOutput struct {
	Decision  string `json:"decision,omitempty"`
	Reason    string `json:"reason,omitempty"`
	HookEvent string `json:"hook_event"`
	InstallID string `json:"install_id,omitempty"`
	Result    string `json:"stop_result"`
	Rule      string `json:"stop_rule,omitempty"`
	Excerpt   string `json:"stop_excerpt,omitempty"`
	Launches  int    `json:"stop_launches,omitempty"`
	Error     string `json:"stop_error,omitempty"`
}

// parseInstallID extracts the value of the --install flag from argv.
// Returns empty string when absent. Accepts both `--install X` (two args)
// and `--install=X` (one arg) forms. Silent on malformed input — foci's
// stream parser handles missing install_ids by treating them as "not ours".
func parseInstallID(args []string) string {
	return parseFlag(args, installIDFlag)
}

// parseFlag returns the value of flag from argv in either `flag X` or
// `flag=X` form, or "" when absent.
func parseFlag(args []string, flag string) string {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		eq := flag + "="
		if len(a) > len(eq) && a[:len(eq)] == eq {
			return a[len(eq):]
		}
	}
	return ""
}

func main() {
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return // exit 0 — silent drop, don't interfere with the turn
	}
	out, ok := handle(os.Args, body)
	if !ok {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

// handle reduces one CC hook envelope to the object main writes: a
// stopOutput for Stop, a hookOutput for the tool events. ok is false when
// the envelope doesn't parse (silent drop).
func handle(args []string, body []byte) (any, bool) {
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, false
	}
	if in.HookEventName == eventStop {
		return stopFor(args, in), true
	}
	return toolOutputFor(args, in), true
}

// stopFor evaluates the stop rules foci passed via --stop-rules. With no
// rules, or rules that don't decode, it passes: a broken rule set must never
// hold a turn open.
func stopFor(args []string, in hookInput) stopOutput {
	out := stopOutput{HookEvent: eventStop, InstallID: parseInstallID(args), Result: stoprule.ResultPass}
	enc := parseFlag(args, stopRulesFlag)
	if enc == "" {
		return out
	}
	rules, err := stoprule.Decode(enc)
	if err != nil {
		out.Error = "decode stop rules: " + err.Error()
		return out
	}
	v := stoprule.Evaluate(rules, stoprule.Input{
		Text:           in.LastAssistantMessage,
		StopHookActive: in.StopHookActive,
		TranscriptPath: in.TranscriptPath,
		PromptID:       in.PromptID,
	})
	out.Result, out.Rule, out.Excerpt, out.Launches, out.Error = v.Result, v.Rule, v.Excerpt, v.Launches, v.Err
	if v.Block() {
		out.Decision, out.Reason = "block", v.Reason
	}
	return out
}

// toolOutputFor reduces a tool-event envelope (PreToolUse, PostToolUse,
// PostToolUseFailure, PermissionDenied) to the hookOutput main writes. A
// PermissionDenied is reported as an errored call carrying the classifier's
// reason; it never gets a hookSpecificOutput, whose only field for that event
// is a retry request.
func toolOutputFor(args []string, in hookInput) hookOutput {
	out := hookOutput{
		HookEvent: in.HookEventName,
		InstallID: parseInstallID(args),
		ToolUseID: in.ToolUseID,
		ToolName:  in.ToolName,
		AgentID:   in.AgentID,
		IsError:   in.HookEventName == "PostToolUseFailure" || in.HookEventName == eventPermissionDenied || in.IsInterrupt || in.IsTimeout,
	}
	if in.HookEventName == "PreToolUse" {
		applyRules(&out, parseFlag(args, rulesFlag), pretool.Call{Tool: in.ToolName, Input: in.ToolInput, Cwd: in.Cwd})
	}
	if len(in.ToolInput) > 0 {
		// Forward the raw tool_input JSON so downstream nudge rules can match
		// on any field (Bash.command, Read.file_path, Grep.pattern, etc.).
		// Truncate to the same maxFieldBytes cap as tool_response — Write/Edit
		// content is the realistic outlier; everything else is comfortably
		// under 1KB, and nudge matching only reads small fields.
		out.ToolInput = truncate(string(in.ToolInput), maxFieldBytes)
	}
	if len(in.ToolResponse) > 0 {
		out.ToolResponse = truncate(decodeToolResponse(in.ToolResponse), maxFieldBytes)
	}
	errText := in.Error
	if in.Reason != "" {
		errText = in.Reason
	}
	if errText != "" {
		out.Error = truncate(errText, maxFieldBytes)
	}
	applyRuntime(&out, in)
	return out
}

// applyRuntime tells the model how long a slow tool call took, as
// PostToolUse(Failure) additionalContext: CC attaches it right after the
// tool's result in the same turn, so there is no foci-side injection to race
// the next API call. PreToolUse carries no duration_ms, so it never matches.
func applyRuntime(out *hookOutput, in hookInput) {
	out.DurationMS = in.DurationMS
	d := time.Duration(in.DurationMS) * time.Millisecond
	if d < slowToolThreshold {
		return
	}
	out.RuntimeShown = true
	out.HookSpecificOutput = &hookSpecific{
		HookEventName: in.HookEventName,
		AdditionalContext: fmt.Sprintf("Tool runtime: this %s call took %s. (Runtimes are reported only for calls taking %s or more.)",
			in.ToolName, d.Round(time.Second), slowToolThreshold),
	}
}

// applyRules marks out as a deny when an encoded pretool rule matches the
// call. Undecodable rules are ignored: a broken rule set must fail open to
// CC's normal permission flow rather than block every tool.
func applyRules(out *hookOutput, encoded string, call pretool.Call) {
	if encoded == "" {
		return
	}
	rules, err := pretool.Decode(encoded)
	if err != nil {
		return
	}
	res := pretool.Match(rules, call)
	// Each error is short: runWhen keeps at most 512 bytes of stderr.
	for _, e := range res.WhenErrors {
		out.WhenErrors = append(out.WhenErrors, e.Error())
	}
	r := res.Rule
	if r == nil {
		return
	}
	out.DeniedRule = r.Name
	out.HookSpecificOutput = &hookSpecific{
		HookEventName:            "PreToolUse",
		PermissionDecision:       pretool.ActionDeny,
		PermissionDecisionReason: r.Reason,
	}
}

// truncate caps s at max bytes, appending a visible marker when it had to
// cut. We cut on byte boundaries, not rune boundaries — the receiving
// parser treats the field as an opaque string so we don't need to worry
// about splitting multi-byte UTF-8 sequences.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// decodeToolResponse turns the raw tool_response RawMessage into a plain
// string for downstream display. CC encodes most file/text tool results as
// JSON strings (e.g. `"contents of the file"`), and forwarding the raw
// bytes verbatim would leave the surrounding double quotes in the user-
// visible "Show full" expansion. We try to unmarshal as a string first;
// when that fails (objects, arrays, numbers — Bash structured output for
// example), we fall back to the raw bytes so the tracker still gets
// something legible.
func decodeToolResponse(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
