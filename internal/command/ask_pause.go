package command

// ask_pause.go — /pause and /resume for the foci-native `ask` mechanism.
//
// While a `foci_ask` is pending, the inbound path captures the user's typed
// replies as the ask's "Other" answer (see internal/agent/run_turn.go). /pause
// suspends that capture: replies run as normal turns and reach the agent, while
// the ask stays pending (buttons still resolve it). /resume restores capture.
//
// Both commands only make sense while an ask is pending, so Visible hides them
// otherwise — but Visible only controls listing, not executability, so each
// Execute re-checks and reports a no-op ("No active question.") when nothing is
// pending. State lives on the AskRouter (internal/tools/ask.go); this layer is
// pure wiring.

import "context"

// askPending reports whether the session has an in-flight ask. Used to gate
// /pause and /resume visibility (cosmetic) and their no-op message (real).
func askPending(cc CommandContext, sessionKey string) bool {
	if cc.Agent == nil || cc.Agent.AskRouter == nil || cc.Agent.AskRouter.PendingForSession == nil {
		return false
	}
	return cc.Agent.AskRouter.PendingForSession(sessionKey) != ""
}

// askToggleCommand builds a /pause- or /resume-style command. toggle selects the
// AskRouter method (PauseSession / ResumeSession); it returns false when no ask
// is pending, which both gates the success message and produces the no-op reply.
func askToggleCommand(name, desc, okMsg string, toggle func(*CommandContext, string) bool) *Command {
	return &Command{
		Name:        name,
		Description: desc,
		Category:    "session",
		Visible: func(_ context.Context, req Request, cc CommandContext) bool {
			return askPending(cc, req.SessionKey)
		},
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			if !toggle(&cc, req.SessionKey) {
				return Response{Text: "No active question."}, nil
			}
			return Response{Text: okMsg}, nil
		},
	}
}

// PauseCommand suspends answer-capture for the session's pending ask.
func PauseCommand() *Command {
	return askToggleCommand("pause",
		"Pause the active question — your typed replies go to the agent instead of answering it",
		"⏸ Question paused — your messages now go to the agent as normal. /resume to answer it again.",
		func(cc *CommandContext, sk string) bool {
			return cc.Agent != nil && cc.Agent.AskRouter != nil &&
				cc.Agent.AskRouter.PauseSession != nil && cc.Agent.AskRouter.PauseSession(sk)
		})
}

// ResumeCommand restores answer-capture for the session's pending ask.
func ResumeCommand() *Command {
	return askToggleCommand("resume",
		"Resume the paused question — your typed replies answer it again",
		"▶ Question resumed — your typed replies answer it again.",
		func(cc *CommandContext, sk string) bool {
			return cc.Agent != nil && cc.Agent.AskRouter != nil &&
				cc.Agent.AskRouter.ResumeSession != nil && cc.Agent.AskRouter.ResumeSession(sk)
		})
}
