package agent

import (
	"context"
	"errors"
	"slices"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/tools"
	"foci/internal/usercmd"
)

// errUserCommandsUnavailable is the Check answer of an agent with no runner.
var errUserCommandsUnavailable = errors.New(usercmd.RefusedUnavailable)

// CheckUserCommand reports why a chat "!" command must not run, or nil.
func (a *Agent) CheckUserCommand(script string) error {
	if a.UserShell == nil {
		return errUserCommandsUnavailable
	}
	return a.UserShell.Check(script)
}

// UserCommandRunnable reports whether word can start a "!" command of this agent
// (#2878, #2882). An agent with no runner has none.
func (a *Agent) UserCommandRunnable(word string) bool {
	return a.UserShell != nil && a.UserShell.Runnable(word)
}

// RedactUserCommand scrubs secret values from a command's own text before it is
// shown anywhere.
func (a *Agent) RedactUserCommand(script string) string {
	if a.UserShell == nil {
		return script
	}
	return a.UserShell.Redact(script)
}

// UserCommandRefusal applies the access rule of a chat "!" command (#2812 req
// 2): refused in every group chat; when the platform has operators the sender
// must be one; otherwise the platform's allow list, already applied by the
// receive path, is the whole rule. The same people may run every slash command.
// The app never reaches this: paired devices are not operator-gated.
func (a *Agent) UserCommandRefusal(platformID, userID string, isGroup bool) string {
	if isGroup {
		return usercmd.RefusedGroup
	}
	if a.UserCommandOperators != nil {
		if ops := a.UserCommandOperators(platformID); len(ops) > 0 && !slices.Contains(ops, userID) {
			return usercmd.RefusedNotOperator
		}
	}
	return ""
}

// RunUserCommand runs script for sessionKey and decides what the agent gets: the
// same size rules and code as an API shell tool result (guardToolResult). persist,
// when non-nil, moves an over-limit saved file to where it should live and returns
// its new path; on error the file stays where it is.
func (a *Agent) RunUserCommand(ctx context.Context, sessionKey, script string, persist func(path string) (string, error)) usercmd.Result {
	if a.UserShell == nil {
		return usercmd.Result{Command: script, State: usercmd.StateFailed, Detail: usercmd.RefusedUnavailable}
	}
	res := a.UserShell.Run(tools.WithSessionKey(ctx, sessionKey), script)
	if res.File != "" && persist != nil {
		if path, err := persist(res.File); err != nil {
			a.logger().Warnf("session=%s keep shell output %s: %v", sessionKey, res.File, err)
		} else {
			res.File = path
		}
	}
	res.ForAgent = res.Output
	if res.File != "" {
		// A cancel must not also cancel the summary of what ran before it.
		gctx := context.WithoutCancel(ctx)
		var client provider.Client
		if a.ResultSummariser == nil {
			client, _, _ = a.ResolveCallSite(config.CallSummarizeTool, sessionKey)
		}
		res.ForAgent = a.guardToolResult(gctx, client, sessionKey, "shell", "",
			tools.ToolResult{Text: res.Output, ResultFile: res.File, ResultSize: res.Size}, nil)
	}
	return res
}

// EnqueueUserCommandResult hands a finished command to the agent as a user-side
// message in base's session, which starts a turn, or joins a running turn like
// any message queued mid-turn. Automated keeps it out of the typed-answer
// captures: a command's output never answers a pending ask for the human.
func (a *Agent) EnqueueUserCommandResult(base Envelope, res usercmd.Result) bool {
	base.Text = res.ForAgentText()
	base.Attachments = nil
	base.Human = true
	base.Automated = true
	base.ReceivedAt = time.Now()
	return a.Enqueue(base)
}

// HandleTextCommand is the whole flow of a "!" command on a text platform
// (Telegram, Discord, Slack): refuse, or run it in the background, reply with
// the result in plain text, and start the agent's turn. base is the envelope the
// message would have been, Text aside. reply sends one plain-text message to the
// chat the command came from.
func (a *Agent) HandleTextCommand(platformID string, base Envelope, script string, reply func(string)) {
	if why := a.UserCommandRefusal(platformID, base.UserID, base.IsGroupChat); why != "" {
		reply(why)
		return
	}
	if err := a.CheckUserCommand(script); err != nil {
		reply(refusalText(err))
		return
	}
	go func() {
		res := a.RunUserCommand(context.Background(), base.SessionKey, script, nil)
		reply(res.ReplyText())
		if !a.EnqueueUserCommandResult(base, res) {
			a.logger().Warnf("session=%s inbox refused a shell command result", base.SessionKey)
		}
	}()
}

// refusalText is the short reply for a Check refusal.
func refusalText(err error) string {
	if errors.Is(err, errUserCommandsUnavailable) {
		return usercmd.RefusedUnavailable
	}
	return "Shell command refused: " + err.Error()
}

// UserCommandRefusalText exposes refusalText to transports that frame their own
// reply.
func UserCommandRefusalText(err error) string { return refusalText(err) }
