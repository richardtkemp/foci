package ccstream

import (
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// The built-in Claude Code tools a restricted batch session gets (--tools).
// No Agent/Task, Skill or AskUserQuestion.
var (
	// isolatedBatchTools: the file tools (confined to the working directory
	// by --restricted) and web access — the API raw spawn's tool set. No
	// Bash and no other code-running tool, so foci's exec-bridge shell
	// functions are out of reach too.
	isolatedBatchTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "WebSearch", "WebFetch"}
	// readOnlyPreapproved: reading and searching files anywhere the agent
	// can, and web access — pre-approved, nothing that writes.
	readOnlyPreapproved = []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch"}
	// readOnlyBatchTools: those, plus a shell whose every command foci
	// decides (#2702): never pre-approved, always asked (readOnlyBashAsk).
	readOnlyBatchTools = append(append([]string(nil), readOnlyPreapproved...), "Bash")
)

// blockedCCTools are the Claude Code built-ins no foci session gets (#2934,
// Dick 2026-11-07: "Block everywhere"): the subagent tool, under its current
// name and its old one. Delegation goes through foci_spawn. Passed as
// --disallowedTools, a deny rule CC applies before any allow rule, setting or
// mode; probed on claude 2.1.314: with the flag the tool is gone from the
// init event's tool list. A depth leaf also keeps its PreToolUse deny
// (depthleaf.go), in case a CC version lists the tool anyway.
var blockedCCTools = []string{"Agent", "Task"}

// readOnlyBashAsk is the settings permission rule that makes Claude Code ask
// foci about EVERY Bash call of a read-only batch (#2702). Without it CC runs
// the commands its own classifier judges read-only (ls, jq, find, cat) with
// no ask, so foci's read-only set would not be the only judge. Probed on
// claude 2.1.314 (--print, haiku, no prompt tool so an ask is a deny):
// without the rule `ls` and `jq` ran; with it both were asked, even with
// `Bash` and `Bash(ls:*)` in --allowedTools (an ask rule beats an allow
// rule, so a user-settings allow cannot pre-approve one either).
var readOnlyBashAsk = map[string]any{"ask": []string{"Bash"}}

// restrictedBatch reports whether a session is a batch whose access narrows
// its tools (spawn raw / explore). Keyed on access alone: only a batch
// carries a non-default access, and a spawn whose asks go up to its spawner
// (#2684, no SkipPermissions) is restricted all the same.
func restrictedBatch(access delegator.BatchAccess) bool {
	return access == delegator.BatchAccessIsolated || access == delegator.BatchAccessReadOnly
}

// restrictedBatchArgs returns the launch flags of a restricted batch:
//   - isolated (verified against claude 2.1.311): --restricted removes the
//     code-running tools and confines the file tools to the working
//     directory; --tools narrows the built-ins to isolatedBatchTools; dontAsk
//     denies anything not pre-approved, with only those tools pre-approved —
//     so CC itself refuses the rest and raises no ask;
//   - read-only (#2702, claude 2.1.314): --tools narrows the built-ins to
//     readOnlyBatchTools; the reading tools are pre-approved and Bash is
//     not, in default mode, so a Bash call is an ask (readOnlyBashAsk makes
//     it one every time) and reaches foci, which decides it from the
//     read-only rules alone (answerUnattended; the batch is unattended);
//   - both: --strict-mcp-config loads no MCP server.
//
// --allowedTools is variadic and ends the list: nothing positional may follow.
func restrictedBatchArgs(access delegator.BatchAccess) []string {
	if access == delegator.BatchAccessIsolated {
		list := strings.Join(isolatedBatchTools, ",")
		return []string{
			"--restricted",
			"--tools", list,
			"--strict-mcp-config",
			"--permission-mode", "dontAsk",
			"--allowedTools", list,
		}
	}
	return []string{
		"--tools", strings.Join(readOnlyBatchTools, ","),
		"--strict-mcp-config",
		"--permission-mode", "default",
		"--allowedTools", strings.Join(readOnlyPreapproved, ","),
	}
}

// readOnlyBatchSettings returns the --settings JSON a read-only batch
// launches with: hookSettings (foci's hook settings, "" when the hooks were
// not installed) with readOnlyBashAsk added as its permissions. One
// --settings carries both: the session must never launch without the ask
// rule, so a failure here fails the launch.
func readOnlyBatchSettings(hookSettings string) (string, error) {
	top := map[string]any{}
	if hookSettings != "" {
		if err := json.Unmarshal([]byte(hookSettings), &top); err != nil {
			return "", fmt.Errorf("read-only batch settings: parse hook settings: %w", err)
		}
	}
	if _, ok := top["permissions"]; ok {
		return "", fmt.Errorf("read-only batch settings: hook settings already carry permissions")
	}
	top["permissions"] = readOnlyBashAsk
	body, err := json.Marshal(top)
	if err != nil {
		return "", fmt.Errorf("read-only batch settings: %w", err)
	}
	return string(body), nil
}
