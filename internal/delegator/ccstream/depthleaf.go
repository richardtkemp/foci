package ccstream

import (
	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
)

// ---------------------------------------------------------------------------
// Depth leaves (#2625): no subagent path but foci_spawn
//
// In a depth leaf (StartOptions.DepthLeaf) the CC process gets
// depthLeafRules, PreToolUse deny rules ahead of the agent's own, so the model
// gets DepthLeafRefusal as the tool result: the Agent tool (its old name Task
// too) in the foreground and the background, Monitor, Workflow, a Bash call
// with run_in_background, and a shell command that detaches itself (`&`,
// nohup, setsid --fork, disown). No other session gets them.
//
// CC's own background switch (CLAUDE_CODE_DISABLE_BACKGROUND_TASKS) is
// deliberately NOT set (Dick, 2026-11-01: "I don't really want anything
// getting killed"): with it, a foreground command that reaches its timeout is
// killed. Without it, CC still moves such a command to the background, as in
// any other session.
//
// Both are decided at each Start, so a leaf keeps them across a respawn or a
// gateway restart; a root, ordinary chat, branch or batch never gets them.
// ---------------------------------------------------------------------------

// depthLeafRulePrefix starts the name of every depth-leaf rule.
const depthLeafRulePrefix = "depth_leaf_"

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
