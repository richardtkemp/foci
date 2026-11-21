package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/session"
)

// /agents delete NAME (aliases rm, remove; #3210): delete an agent inside the
// running gateway, behind TWO confirmations (Dick, 2026-11-21). This file
// parses and applies the refusals that need no config; the gateway does the
// rest (AgentDeleteFn): the plan, both confirmations, the live stop and the
// disk delete. Rulings (Dick, 2026-11-21, on the ticket):
//   - an agent's own chat cannot delete that agent (the result needs a chat
//     that still exists);
//   - the last agent cannot be deleted (gateway: it needs the config);
//   - the HTTP API (`foci command`, an agent's shell) cannot delete an agent:
//     an agent never deletes an agent;
//   - in the app the second confirmation is the app-wide dialog with the big
//     red warning; Telegram/Discord get two button messages.

// AgentDeleteArgs is one parsed /agents delete.
type AgentDeleteArgs struct {
	ID      string
	Session string // the requesting session: the confirmations and the result go to its chat
	Source  string // Request.Source: "app" shows the app-wide dialogs
}

// AgentDeleteFn carries out a parsed /agents delete and returns the reply.
type AgentDeleteFn func(ctx context.Context, a AgentDeleteArgs) string

// agentDeleteVerbs are the subcommand and its aliases.
var agentDeleteVerbs = map[string]bool{"delete": true, "rm": true, "remove": true}

// agentDeleteUsage is the reply to a malformed /agents delete.
const agentDeleteUsage = "Usage: /agents delete NAME (aliases: rm, remove)\n" +
	"Deletes agent NAME after two confirmations: its config block, secrets, crontab lines, sessions and stored rows " +
	"(after a backup), and its workspace and transcripts (to the trash). Its cost and usage ledger rows are kept."

// agentsDelete handles "/agents delete …" (args: the text after the verb).
func agentsDelete(ctx context.Context, req Request, cc CommandContext, args string) Response {
	f := strings.Fields(args)
	if len(f) != 1 || strings.HasPrefix(f[0], "-") {
		return Response{Text: agentDeleteUsage}
	}
	id := f[0]
	if fromHTTP(req) {
		return Response{Text: "/agents delete cannot run from the HTTP API (`foci command`, an agent's shell): an agent never deletes an agent. Send it yourself in chat or the app."}
	}
	if session.AgentIDFromAnyKey(req.SessionKey) == id {
		return Response{Text: fmt.Sprintf("Agent %q cannot delete itself from its own chat: send /agents delete %s from another agent's chat, so the result has a chat to go to. Nothing changed.", id, id)}
	}
	if cc.AgentDeleteFn == nil {
		return Response{Text: "Agent delete is not available."}
	}
	return Response{Text: cc.AgentDeleteFn(ctx, AgentDeleteArgs{ID: id, Session: req.SessionKey, Source: req.Source})}
}
