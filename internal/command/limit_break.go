package command

import (
	"context"

	"foci/internal/agent"
	"foci/internal/platform"
)

// LimitBreakName is the registry name of /limit_break (#3128).
const LimitBreakName = "limit_break"

// LimitBreakCommand returns /limit_break, alias /lb (#3128, Dick 2026-11-15:
// "a new command /limit-break alias /lb to force through a message past the
// limit. E.g. /lb hey one last thing real quick..."). "/lb <text>" runs <text>
// as a normal user turn now, although the usage hold is on; the hold stays on
// for everything else.
//
// A message with text never reaches Execute on a chat surface: the surfaces
// pass it to the agent as a message (dispatch.IsLimitBreakMessage), and the
// agent strips the word and lets the turn past the hold (agent.Enqueue). The
// command is registered so it is listed and handles a bare /lb (#3182, Dick
// 2026-11-19: "If not given any additional parameters it should operate on
// the previous message sent by the user, releasing the hold so it sends to
// the backend immediately"; 13:02: "Bare lb to show usage info if there is
// currently no limit to break"; 13:40: "should send *every* held message in
// the current session"): with the hold on, every held user message of the
// chat runs now, oldest first (agent.LimitBreakHeld), or the reply says
// nothing is held; with no hold, the reply is the usage.
//
// Registered for a backend that reports usage (CapUsageReports: Claude Code
// and codex), the backends whose account a usage hold can hold.
func LimitBreakCommand() *Command {
	return &Command{
		Name:        LimitBreakName,
		Aliases:     []string{"lb", "limit-break"},
		Description: "Run one message now, past the usage hold: /lb <message>, or a bare /lb to run every held message",
		Category:    "operations",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			if req.Args == "" {
				if cc.Agent == nil || req.SessionKey == "" {
					return Response{Text: platform.LimitBreakUsage}, nil
				}
				switch cc.Agent.LimitBreakHeld(req.SessionKey) {
				case agent.LimitBreakReleased:
					return Response{Text: platform.LimitBreakReleased}, nil
				case agent.LimitBreakNothingHeld:
					return Response{Text: platform.LimitBreakNothingHeld}, nil
				default:
					return Response{Text: platform.LimitBreakUsage}, nil
				}
			}
			// Only a source that does not pass the message on gets here
			// (the HTTP /command endpoint): nothing runs.
			return Response{Text: "/lb runs a message only when typed in a chat; nothing was sent."}, nil
		},
	}
}
