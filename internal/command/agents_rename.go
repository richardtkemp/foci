package command

import (
	"context"
	"strings"
)

// /agents rename OLD NEW [--move-workspace] [--yes] (#2787): rename an agent
// inside the running gateway. Only OLD is interrupted; every other agent runs
// on. Typed in chat or the app (the user, who owns the install), it first
// shows the plan and runs on --yes. Arriving over the HTTP API (`foci command`,
// an agent's shell), it never runs directly: it asks the owner to approve it
// with an Approve/Deny prompt, as `/config set` over HTTP does (#479). The
// gateway does the work (AgentRenameFn); this file parses and routes.

// AgentRenameArgs is one parsed /agents rename.
type AgentRenameArgs struct {
	Old, New string
	Move     bool   // --move-workspace: move ~/OLD to ~/NEW (default: keep it)
	Yes      bool   // --yes: run it (chat/app); without it, show the plan
	FromHTTP bool   // arrived over the HTTP API: ask the owner to approve
	Session  string // the requesting session: approval prompts and the result go to its chat
}

// AgentRenameFn carries out a parsed /agents rename and returns the reply.
type AgentRenameFn func(ctx context.Context, a AgentRenameArgs) string

// agentRenameUsage is the reply to a malformed /agents rename.
const agentRenameUsage = "Usage: /agents rename OLD NEW [--move-workspace] [--yes]\n" +
	"Renames agent OLD to NEW in the running gateway: only OLD stops, for the time the rename takes. " +
	"Without --yes it shows the plan. --move-workspace also moves ~/OLD to ~/NEW (default: keep the workspace where it is)."

// parseAgentRename parses the arguments after "rename".
func parseAgentRename(args string) (AgentRenameArgs, bool) {
	var a AgentRenameArgs
	var ids []string
	for _, f := range strings.Fields(args) {
		switch f {
		case "--move-workspace":
			a.Move = true
		case "--yes", "-y":
			a.Yes = true
		default:
			if strings.HasPrefix(f, "-") {
				return a, false
			}
			ids = append(ids, f)
		}
	}
	if len(ids) != 2 {
		return a, false
	}
	a.Old, a.New = ids[0], ids[1]
	return a, true
}

// agentsRename handles "/agents rename …" (args: the text after "rename").
func agentsRename(ctx context.Context, req Request, cc CommandContext, args string) Response {
	a, ok := parseAgentRename(args)
	if !ok {
		return Response{Text: agentRenameUsage}
	}
	if cc.AgentRenameFn == nil {
		return Response{Text: "Agent rename is not available."}
	}
	a.FromHTTP = fromHTTP(req)
	a.Session = req.SessionKey
	return Response{Text: cc.AgentRenameFn(ctx, a)}
}
