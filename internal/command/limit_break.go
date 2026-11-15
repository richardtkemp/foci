package command

import (
	"context"

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
// command is registered so it is listed and so a bare /lb gets its usage.
//
// Registered for a backend that reports usage (CapUsageReports: Claude Code
// and codex), the backends whose account a usage hold can hold.
func LimitBreakCommand() *Command {
	return &Command{
		Name:        LimitBreakName,
		Aliases:     []string{"lb", "limit-break"},
		Description: "Run one message now, past the usage hold: /lb <message>",
		Category:    "operations",
		Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
			if req.Args == "" {
				return Response{Text: platform.LimitBreakUsage}, nil
			}
			// Only a source that does not pass the message on gets here
			// (the HTTP /command endpoint): nothing runs.
			return Response{Text: "/lb runs a message only when typed in a chat; nothing was sent."}, nil
		},
	}
}
