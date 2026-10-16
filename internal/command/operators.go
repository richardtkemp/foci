package command

import (
	"fmt"
	"sort"
	"strings"

	"foci/internal/config"
	"foci/internal/log"
)

// OperatorPolicies maps a chat transport (Request.Source — a Source* constant,
// which is also the [[platforms]] id) to the operator policy of one agent on
// that transport. An entry exists iff the agent has operators there, so a
// transport with no entry is never gated. Surfaces that carry no policy entry
// and are therefore NOT gated (#2279):
//
//   - the app (FAP) transport, which stamps Source "app" (internal/app) —
//     paired devices are the operator's own and pairing is gated by
//     allowed_devices;
//   - the authenticated HTTP endpoints /send, /command, /status, the deferred
//     command sweep and the suggestion action buttons, all of which dispatch
//     via runAgentCommand and stamp Source "http" (cmd/foci-gw);
//   - the Telegram bot menu, which lists with an empty Request (no source);
//   - wizard replies and im:/wz: button answers (#2302 and the approval
//     target — separate features).
//
// The policy rides inside CommandContext, so every bot that receives the
// agent's CommandContext — primary, pooled facet, restored facet, and every
// dispatcher rebuilt from it — is restricted identically.
type OperatorPolicies map[string]*OperatorPolicy

// NewOperatorPolicies resolves and builds the operator policy of every
// operator-gated chat transport (telegram, discord) for one agent. reg must be
// fully registered: command list entries are resolved against it. Returns nil
// when the agent has no operators anywhere, which gates nobody.
func NewOperatorPolicies(reg *Registry, cfg *config.Config, acfg config.AgentConfig) OperatorPolicies {
	var out OperatorPolicies
	for _, platformID := range []string{SourceTelegram, SourceDiscord} {
		access := config.ResolveOperatorAccess(cfg, acfg, platformID)
		if len(access.Users) == 0 {
			continue // no operators on this transport: nobody is restricted
		}
		if out == nil {
			out = OperatorPolicies{}
		}
		out[platformID] = NewOperatorPolicy(reg, acfg.ID, platformID, access)
	}
	return out
}

// Forbids reports whether the caller may not run cmd with args (the text
// after the command name, which may name one of its subcommands): nil map, a
// missing source entry, or a policy with no operators configured never
// forbids.
func (o OperatorPolicies) Forbids(c Caller, cmd *Command, args string) bool {
	p := o[c.Source]
	return p != nil && p.Forbids(c.UserID, cmd, args)
}

// Hides reports whether the caller may run no form of cmd — neither the
// command itself nor any of its subcommands — so listings (/help, "Did you
// mean") leave it out. A command with only some forms permitted stays listed.
func (o OperatorPolicies) Hides(c Caller, cmd *Command) bool {
	p := o[c.Source]
	return p != nil && p.hides(c.UserID, cmd)
}

// refusal reports the operator-only reply for a command (with args) forbidden
// to the caller, logging the refusal at WARN on the agent's logger; refused is
// false when the caller may run it (one decision site: Forbids). This is the
// one refusal site — Dispatch and "/help <command>" call it, and every other
// surface (keyboards, listings) consults Forbids or Hides without logging.
func (o OperatorPolicies) refusal(c Caller, cmd *Command, args string) (reply string, refused bool) {
	if !o.Forbids(c, cmd, args) {
		return "", false
	}
	key := permitKey(cmd, args)
	o[c.Source].refuseLog.Warnf("operator-only command refused: user=%s command=/%s", c.UserID, key)
	return operatorOnlyReply(key), true
}

// OperatorPolicy is the immutable operator gate for one agent on one chat
// platform: the operator user IDs, and the command forms a non-operator may
// run.
type OperatorPolicy struct {
	platform  string
	operators map[string]struct{}
	permitted map[string]struct{} // permitKey values: "reset" or "reset hard"
	unknown   []string            // list entries that named no registered command
	refuseLog *log.ComponentLogger
}

// NewOperatorPolicy builds one platform's policy. Each non_operator_commands
// entry names a command ("reset") or one of its declared subcommands ("reset
// hard"); it is normalised (any case, optional leading /, an alias names its
// command or subcommand) and resolved against reg. An entry naming no
// registered command or subcommand is kept in unknown for LogReport to warn
// about and ignored by the gate.
func NewOperatorPolicy(reg *Registry, agentID, platform string, access config.OperatorAccess) *OperatorPolicy {
	p := &OperatorPolicy{
		platform:  platform,
		operators: make(map[string]struct{}, len(access.Users)),
		permitted: make(map[string]struct{}, len(access.Commands)),
		refuseLog: log.NewComponentLogger("agent:" + agentID),
	}
	for _, id := range access.Users {
		p.operators[id] = struct{}{}
	}
	for _, entry := range access.Commands {
		key, ok := resolveCommandEntry(reg, entry)
		if !ok {
			p.unknown = append(p.unknown, entry)
			continue
		}
		p.permitted[key] = struct{}{}
	}
	return p
}

// resolveCommandEntry maps one non_operator_commands entry to its permitKey: a
// command name, optionally followed by one of that command's declared
// subcommands. ok is false when the command is not registered, the second
// word names no subcommand of it, or there are more than two words.
func resolveCommandEntry(reg *Registry, entry string) (key string, ok bool) {
	words := strings.Fields(entry)
	if len(words) == 0 || len(words) > 2 {
		return "", false
	}
	cmd := reg.Get(CommandKey(words[0]))
	if cmd == nil {
		return "", false
	}
	if len(words) == 2 && cmd.canonical().subcommand(words[1]) == nil {
		return "", false
	}
	return permitKey(cmd, strings.Join(words[1:], " ")), true
}

// permitKey names the form of cmd that args invoke: the canonical command
// name, plus the canonical subcommand name when args' first word names one of
// its declared subcommands ("reset hard"). Any other args (or none) invoke the
// command itself. Both the permitted list and every gate check key through
// this, so an alias, a subcommand alias and case cannot bypass the gate.
//
// The first word is split with strings.Fields — any Unicode whitespace — the
// same tokenisation as subcommand dispatch (buildSubcommandDispatch) and
// IsImmediateText, so "/reset hard\nplease" is judged as the "reset hard" it
// runs. Splitting on spaces alone let a newline or tab after the subcommand
// pass as the bare command (#2279 final review B1).
func permitKey(cmd *Command, args string) string {
	c := cmd.canonical()
	if words := strings.Fields(args); len(words) > 0 {
		if sub := c.subcommand(words[0]); sub != nil {
			return c.Name + " " + sub.Name
		}
	}
	return c.Name
}

// Forbids reports whether userID may not run cmd with args. Nil-receiver
// safe (nil forbids nobody), and a nil cmd is never forbidden — there is
// nothing to run. A user is restricted only when the operator list is
// non-empty and does not contain them — whatever the command list says — and
// an empty user ID is restricted, so an unidentified caller cannot slip past
// the gate. Permission is judged on the exact form args invoke (permitKey):
// "reset" permits /reset but not /reset hard, which needs its own entry; an
// alias entry (a stop alias) follows the command it names.
func (p *OperatorPolicy) Forbids(userID string, cmd *Command, args string) bool {
	if !p.restricts(userID, cmd) {
		return false
	}
	_, ok := p.permitted[permitKey(cmd, args)]
	return !ok
}

// hides reports whether userID may run no form of cmd: not the command
// itself and none of its subcommands.
func (p *OperatorPolicy) hides(userID string, cmd *Command) bool {
	if !p.restricts(userID, cmd) {
		return false
	}
	c := cmd.canonical()
	if _, ok := p.permitted[c.Name]; ok {
		return false
	}
	for _, sub := range c.Subcommands {
		if _, ok := p.permitted[c.Name+" "+sub.Name]; ok {
			return false
		}
	}
	return true
}

// restricts reports whether the gate applies to userID and cmd at all.
func (p *OperatorPolicy) restricts(userID string, cmd *Command) bool {
	if p == nil || len(p.operators) == 0 || cmd == nil {
		return false
	}
	_, isOperator := p.operators[userID]
	return !isOperator
}

// OperatorReportLogger is the logging subset LogReport needs;
// *log.ComponentLogger satisfies it.
type OperatorReportLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// LogReport writes the startup report for this policy: one INFO line naming
// the agent, the platform, the operator count and the permitted commands, and
// one WARN per non_operator_commands entry that named no registered command
// (ignored by the gate). Nil-receiver safe and silent: the report — and the
// WARNs — exist only for a platform where the agent HAS operators, so an
// agent with no operators configured anywhere logs nothing (#2279
// requirement 8).
func (p *OperatorPolicy) LogReport(l OperatorReportLogger, agentID string) {
	if p == nil {
		return
	}
	names := make([]string, 0, len(p.permitted))
	for name := range p.permitted {
		names = append(names, name)
	}
	sort.Strings(names)
	permitted := "(none)"
	if len(names) > 0 {
		permitted = strings.Join(names, ", ")
	}
	l.Infof("agent %q: %s operators=%d, non-operators may run: %s", agentID, p.platform, len(p.operators), permitted)
	for _, entry := range p.unknown {
		l.Warnf("agent %q: %s non_operator_commands: %q names no registered command, ignored", agentID, p.platform, entry)
	}
}

// operatorOnlyReply is the one refusal text a forbidden command answers with,
// naming the refused form by its permitKey (an alias is refused as its
// command, "/reset hard" as itself).
func operatorOnlyReply(key string) string {
	return fmt.Sprintf("/%s is only for this agent's operators.", key)
}
