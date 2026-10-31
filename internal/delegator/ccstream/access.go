package ccstream

import (
	"strings"

	"foci/internal/delegator"
)

// The built-in Claude Code tools a restricted batch session gets (--tools).
// No Bash and no other code-running tool, so foci's exec-bridge shell
// functions are out of reach too; no Agent/Task, Skill or AskUserQuestion.
var (
	// isolatedBatchTools: the file tools (confined to the working directory
	// by --restricted) and web access — the API raw spawn's tool set.
	isolatedBatchTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "WebSearch", "WebFetch"}
	// readOnlyBatchTools: reading and searching files anywhere the agent can,
	// and web access — nothing that writes or runs.
	readOnlyBatchTools = []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch"}
)

// restrictedBatch reports whether a session is a batch whose access narrows
// its tools (spawn raw / explore). Keyed on access alone: only a batch
// carries a non-default access, and a spawn whose asks go up to its spawner
// (#2684, no SkipPermissions) is restricted all the same.
func restrictedBatch(access delegator.BatchAccess) bool {
	return access == delegator.BatchAccessIsolated || access == delegator.BatchAccessReadOnly
}

// restrictedBatchArgs returns the launch flags of a restricted batch
// (verified against claude 2.1.311):
//   - isolated: --restricted removes the code-running tools and confines the
//     file tools to the working directory; --tools narrows the built-ins to
//     isolatedBatchTools;
//   - read-only: --tools narrows the built-ins to readOnlyBatchTools;
//   - both: --strict-mcp-config loads no MCP server, and dontAsk denies
//     anything not pre-approved, with only those tools pre-approved — so CC
//     itself refuses the rest and raises no ask.
//
// --allowedTools is variadic and ends the list: nothing positional may follow.
func restrictedBatchArgs(access delegator.BatchAccess) []string {
	tools := readOnlyBatchTools
	var args []string
	if access == delegator.BatchAccessIsolated {
		tools = isolatedBatchTools
		args = append(args, "--restricted")
	}
	list := strings.Join(tools, ",")
	return append(args,
		"--tools", list,
		"--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--allowedTools", list,
	)
}
