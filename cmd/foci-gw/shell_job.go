package main

// Shell jobs (#3286): the gateway half of tools.ShellJobTool. A depth leaf's
// shell job runs a command in foci, so its command is checked here against the
// agent's own rules (the outer foci_shell_job call is auto-approved like every
// foci_* function, so without this check the tool would be a new privilege
// path), and a command no rule allows is put to the user in the app (Dick
// 2026-11-26: "Ask me in the app").

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator/autoapprove"
	"foci/internal/delegator/pretool"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

// shellJobRules is what a shell job's command is judged by: the agent's own
// rules, as its backend judges a Bash call.
type shellJobRules struct {
	// runner refuses a blocked path or a {{secret:}} template (CheckJob).
	runner *shell.UserRunner
	// autoApprove are the agent's compiled auto-approve rules
	// (buildAutoApproveRules), matched under env: the environment the job's
	// command runs with, so a rule's variable expansion sees what the
	// command will see (autoapprove.MatchWithEnv).
	autoApprove []autoapprove.Rule
	env         map[string]string
	// preTool returns the agent's PreToolUse deny rules (nil: its backend
	// has none, CapPreToolRules); read at each check, so a config edit
	// applies at once.
	preTool func() []pretool.Rule
	// workDir is the command's cwd, for a rule's cwd patterns.
	workDir string
}

// check judges command: refused by a deny rule (a blocked path, a detaching
// command, a PreToolUse deny rule), allowed by an auto-approve rule, or else
// put to the user.
func (r shellJobRules) check(_, command string) tools.ShellJobCheck {
	if r.runner != nil {
		if err := r.runner.CheckJob(command); err != nil {
			return tools.ShellJobCheck{Verdict: tools.ShellJobDeny, Reason: err.Error()}
		}
	}
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return tools.ShellJobCheck{Verdict: tools.ShellJobDeny, Reason: "the command could not be encoded: " + err.Error()}
	}
	deny := shellJobDetachRules()
	if r.preTool != nil {
		deny = append(deny, r.preTool()...)
	}
	if m := pretool.Match(deny, pretool.Call{Tool: "Bash", Input: input, Cwd: r.workDir}); m.Rule != nil {
		return tools.ShellJobCheck{Verdict: tools.ShellJobDeny, Reason: fmt.Sprintf("rule %s refuses it: %s", m.Rule.Name, m.Rule.Reason)}
	}
	ok, veto := autoapprove.MatchWithEnv(r.autoApprove, "Bash", input, r.env)
	if ok {
		return tools.ShellJobCheck{Verdict: tools.ShellJobAllow}
	}
	why := "no auto-approve rule of this agent allows the command"
	if veto != "" {
		why += " (a rule matched, but " + veto + ")"
	}
	return tools.ShellJobCheck{Verdict: tools.ShellJobAsk, Reason: why}
}

// shellJobDetachRules are the deny rules a shell job's command is checked
// against on top of the agent's own: a job may not leave a process running
// after it ends, where nothing tracks, stops or reports it.
func shellJobDetachRules() []pretool.Rule {
	return pretool.DetachRules("shell_job_", func(what string) string {
		return "a shell job may not detach a process from itself (" + what + "): nothing would track, stop or report it once the job ends. Run the command in the job's foreground"
	})
}

// shellJobApprover puts a shell job to the user (#3286): an Allow / Deny
// prompt in the chat of the session that started it, or, when that chat
// cannot show it, the next chat up (promptTargets, as a backend's approval
// prompt goes).
type shellJobApprover struct {
	d backendPromptDeps
}

// shellJobApproveData are the prompt's two choices.
const (
	shellJobAllowData = "allow"
	shellJobDenyData  = "deny"
)

// approve shows the request and waits for the answer, the prompt's expiry,
// or ctx's end (the job was stopped: the prompt is withdrawn).
func (a shellJobApprover) approve(ctx context.Context, req tools.ShellJobApproval) (bool, string) {
	logger := log.NewComponentLogger("agent:" + a.d.agentID)
	reqID := "shelljob-" + req.ID
	type answer struct {
		ok  bool
		why string
	}
	answered := make(chan answer, 1)
	give := func(a answer) {
		select {
		case answered <- a:
		default:
		}
	}
	summary := "Shell job " + req.Name
	onClick := func(c platform.ButtonChoice) string {
		if c.Data == shellJobAllowData {
			give(answer{true, "approved"})
			return "✅ Allowed: " + summary
		}
		give(answer{false, "the user denied it"})
		return "❌ Denied: " + summary
	}
	onExpire := func() { give(answer{false, "the request expired before the user answered it"}) }
	buttons := []platform.ButtonChoice{{Label: "Allow", Data: shellJobAllowData}, {Label: "Deny", Data: shellJobDenyData}}
	body := shellJobPromptText(req)

	var failures []string
	delivered := false
	for _, target := range promptTargets(a.d, req.SessionKey) {
		chatAgent := chatAgentID(target, a.d.agentID)
		resolve := connResolver(a.d.connMgr, target, chatAgent)
		conn := resolve()
		if conn == nil {
			failures = append(failures, target+": no connection")
			continue
		}
		if refuseApproval(logger, conn, req.SessionKey, reqID, summary) {
			return false, "this chat's platform cannot show approval prompts"
		}
		text := prependRequestOrigin(a.d.sessionIdx, chatAgent, conn, req.SessionKey, body)
		if target != req.SessionKey {
			text = "From session " + req.SessionKey + " (its own chat cannot show this request).\n\n" + text
		}
		if _, err := platform.SendInteractiveMessageWithID(resolve, reqID, summary, text, buttons, onClick, onExpire); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", target, err))
			continue
		}
		logger.Infof("shell job=%s session=%s: approval asked in %s", req.ID, req.SessionKey, target)
		delivered = true
		break
	}
	if !delivered {
		logger.Warnf("shell job=%s session=%s: approval could not be shown in any chat (%s) — not run", req.ID, req.SessionKey, strings.Join(failures, "; "))
		return false, "foci could not show the request to the user in any chat (" + strings.Join(failures, "; ") + ")"
	}
	select {
	case r := <-answered:
		return r.ok, r.why
	case <-ctx.Done():
		if err := platform.CancelInteractiveMessage(reqID, "❌ "+summary+": withdrawn, the job was stopped"); err != nil {
			logger.Warnf("shell job=%s: withdraw prompt: %v", req.ID, err)
		}
		return false, "the job was stopped"
	}
}

// shellJobPromptText is the body of a shell job's approval prompt.
func shellJobPromptText(req tools.ShellJobApproval) string {
	return fmt.Sprintf("🐚 **Shell job %q wants to run a command** in the background, with no model, for up to %s:\n\n```\n%s\n```\n\n%s. Allow it?",
		req.Name, req.Timeout, req.Command, upperFirst(req.Why))
}

// upperFirst capitalises s's first byte (an ASCII reason).
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// newShellJobEnv is a delegated agent's tools.ShellJobEnv: rules judge each
// command, approver asks the user, and runner runs it under the job's own
// timeout. nil without a runner.
func newShellJobEnv(runner *shell.UserRunner, rules shellJobRules, approver shellJobApprover) *tools.ShellJobEnv {
	if runner == nil {
		return nil
	}
	return &tools.ShellJobEnv{
		Check:   rules.check,
		Approve: approver.approve,
		Run: func(ctx context.Context, command string, timeout time.Duration) usercmd.Result {
			return runner.RunFor(ctx, command, timeout)
		},
	}
}
