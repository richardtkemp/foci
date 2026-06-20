package command

import (
	"context"
)

// LoginCommand returns a /login command that manually triggers the automated
// Claude Code re-login flow (#843).
//
// Normally the flow fires on its own when the ccstream backend emits a 401
// auth failure. This command exposes the same trigger manually so the flow can
// be exercised on demand (e.g. to test it without waiting for a real token
// expiry). It drives a `claude /login` TUI in tmux, relays the login URL to
// the user, treats the user's next message as the login code, and resumes
// once login succeeds — pausing normal message processing throughout.
//
// RequiresBackend gates it to CC-family agents. The actual trigger is only
// wired for the ccstream backend (Agent.ReloginTrigger is nil for cctmux), so
// the handler reports unavailability rather than mis-driving the wrong backend.
func LoginCommand() *Command {
	return &Command{
		Name:        "login",
		Description: "Manually trigger Claude Code re-login (ccstream only)",
		Category:    "operations",
		Requires:    RequiresBackend,
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.Agent == nil || cc.Agent.ReloginTrigger == nil {
				return Response{Text: "Re-login is only available on the ccstream backend."}, nil
			}
			if !cc.Agent.ReloginTrigger("manual /login command") {
				return Response{Text: "A re-login is already in progress."}, nil
			}
			return Response{Text: "🔐 Re-login started. Message processing is paused — watch for the login URL, then reply with the code from the browser."}, nil
		},
	}
}
