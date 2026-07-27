// Command foci-codex-hook is the helper foci installs as a PreToolUse hook on
// every `codex app-server` it launches. It exists to fix one thing: a shared
// app-server has ONE process environment, so FOCI_SOCK / FOCI_SESSION_KEY /
// BASH_ENV — which foci sets per session — are correct only for whichever
// session started the process. Every other session's bash tool calls would go
// out over the first session's exec bridge and land in the first session's
// chat, silently.
//
// Codex invokes this binary before each tool call, pipes a JSON envelope
// (session_id, tool_name, tool_input, tool_use_id, …) into its stdin, and
// reads a JSON decision from its stdout. session_id IS the codex thread id and
// does vary per thread on a shared app-server, so it is the key foci needs:
// this binary looks up {env-dir}/{session_id}.json — written by foci when it
// bound that thread to a session — and rewrites the command to run under the
// right environment.
//
// It always exits 0. A non-zero exit or a malformed decision is how a codex
// hook BLOCKS a tool call, and a routing helper must never be able to break an
// agent's turn: every failure path here degrades to "emit nothing", which
// leaves codex's own behaviour exactly as it would have been without the hook.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"

	"foci/internal/delegator/sessionenv"
)

// envDirFlag must match the flag internal/delegator/codex/hooks.go bakes into
// the hook command string. foci passes the directory explicitly rather than
// letting this binary resolve tempdir itself: the hook is a grandchild of
// foci-gw via codex, and an inherited FOCI_TMPDIR is not something to bet a
// silent misroute on.
const envDirFlag = "env-dir"

// hookInput is the subset of codex's PreToolUse envelope foci consumes.
// Verified against codex-cli 0.145.0.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// hookOutput is codex's PreToolUse decision envelope. permissionDecision
// "allow" is MANDATORY alongside updatedInput — codex rejects the rewrite
// outright otherwise ("PreToolUse hook returned updatedInput without
// permissionDecision:allow"). It does NOT bypass codex's approval flow:
// with approvalPolicy=untrusted an allow-plus-rewrite still raises
// item/commandExecution/requestApproval (verified live, 2 threads, 0.145.0),
// so foci's permission prompt and auto-approve rules keep running.
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName      string       `json:"hookEventName"`
	PermissionDecision string       `json:"permissionDecision"`
	UpdatedInput       updatedInput `json:"updatedInput"`
}

type updatedInput struct {
	Command string `json:"command"`
}

func main() {
	envDir := flag.String(envDirFlag, "", "directory holding per-thread session-env JSON files")
	flag.Parse()

	if out, ok := decide(os.Stdin, *envDir); ok {
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}
}

// decide reads one hook envelope and returns the rewrite to emit, or ok=false
// to stay silent. Silence is the correct answer for every case foci does not
// positively need to act on: a non-Bash tool, a thread foci has no binding
// for, an unreadable or malformed env file, or a payload we can't parse.
func decide(r io.Reader, envDir string) (hookOutput, bool) {
	var in hookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return hookOutput{}, false
	}
	if in.ToolName != "Bash" || in.SessionID == "" || in.ToolInput.Command == "" {
		return hookOutput{}, false
	}
	entry, err := sessionenv.Load(envDir, in.SessionID)
	if err != nil || entry.IsZero() {
		return hookOutput{}, false
	}
	wrapped := sessionenv.WrapCommand(in.ToolInput.Command, entry)
	if wrapped == in.ToolInput.Command {
		return hookOutput{}, false
	}
	return hookOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:      "PreToolUse",
		PermissionDecision: "allow",
		UpdatedInput:       updatedInput{Command: wrapped},
	}}, true
}
