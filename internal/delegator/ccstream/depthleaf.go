package ccstream

import (
	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
)

// ---------------------------------------------------------------------------
// Depth leaves (#2625): no subagent path but foci_spawn
//
// In a depth leaf (StartOptions.DepthLeaf) the CC process gets two extra
// guards, and no other session does:
//
//  1. CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 on its env. CC then never runs a
//     shell in the background: the Bash schema drops run_in_background (a call
//     that still passes it fails CC's input validation, so it never runs), a
//     foreground command that reaches its timeout is killed instead of being
//     moved to the background, and the Agent tool cannot run in the
//     background. Verified on CC 2.1.314 with a throwaway probe: a
//     run_in_background call came back "InputValidationError: An unexpected
//     parameter `run_in_background` was provided" and never reached the
//     PreToolUse hook; the control arm without the variable backgrounded it
//     (backgroundTaskId). In the bundle, every auto-background branch of the
//     Bash tool is gated on the same check. It goes on the env after the
//     per-agent env, so backend_config.env cannot switch it off.
//  2. depthLeafRules, PreToolUse deny rules ahead of the agent's own, so the
//     model gets DepthLeafRefusal as the tool result: the Agent tool (its old
//     name Task too) in the foreground and the background, Monitor, Workflow,
//     a Bash call with run_in_background (kept for a CC that accepts the
//     field again), and a shell command that detaches itself (`&`, nohup,
//     setsid --fork, disown).
//
// Both are decided at each Start, so a leaf keeps them across a respawn or a
// gateway restart; a root, ordinary chat, branch or batch never gets them.
// ---------------------------------------------------------------------------

// disableBackgroundTasksEnv is CC's switch that turns off every background
// task (shells and agents).
const disableBackgroundTasksEnv = "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"

// depthLeafRulePrefix starts the name of every depth-leaf rule.
const depthLeafRulePrefix = "depth_leaf_"

// depthLeafEnv is the extra CC process env of a depth leaf.
func depthLeafEnv() map[string]string {
	return map[string]string{disableBackgroundTasksEnv: "1"}
}

// depthLeafRules are the PreToolUse deny rules of a depth leaf. They are not
// part of any config layer, so no config can disable them.
func depthLeafRules() []pretool.Rule {
	deny := func(name, tool, path string) pretool.Rule {
		return pretool.Rule{Name: depthLeafRulePrefix + name, Tool: tool, Action: pretool.ActionDeny,
			Reason: delegator.DepthLeafRefusal(path)}
	}
	yes := true
	bgShell := "A background shell command"
	rules := []pretool.Rule{
		deny("agent", "Agent", "The Agent tool (foreground or background)"),
		deny("task", "Task", "The Task tool (foreground or background)"),
		deny("monitor", "Monitor", "The Monitor tool"),
		deny("workflow", "Workflow", "The Workflow tool"),
	}
	inBg := deny("bash_run_in_background", "Bash", bgShell+" (run_in_background)")
	inBg.Input = map[string]pretool.Patterns{"run_in_background": {`^true$`}}
	amp := deny("bash_ampersand", "Bash", bgShell+" (a command ended by &)")
	amp.Background = &yes
	detach := deny("bash_detach", "Bash", bgShell+" (nohup, setsid --fork or disown)")
	detach.Command = pretool.Patterns{
		`(\S*/)?(nohup|disown)(\s|$)`,
		`(\S*/)?setsid\s+(\S+\s+)*(--fork|-[A-Za-z]*f[A-Za-z]*)(\s|$)`,
	}
	return append(rules, inBg, amp, detach)
}
