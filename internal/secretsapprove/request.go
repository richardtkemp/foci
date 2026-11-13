// Package secretsapprove runs agent-requested changes to secrets.toml past the
// user (#3090). An agent never sees or handles a secret value. It asks
// instead (`foci_secrets set|remove|hosts|body|agents …`), and the user
// answers in a foci app dialog: Approve or Deny, and for a set, the value,
// typed in the dialog. The value goes from the app straight to the secret
// store; it never reaches the agent, its transcript, or a log. The agent
// learns only the outcome.
//
// The actions mirror the user's /secrets command (list, set, remove, hosts,
// body), plus the per-section agent rules (allowed_agents / denied_agents),
// which /secrets cannot edit. list needs no dialog (Dick, 2026-11-13): it
// shows names and rules, never values.
//
// Requests live in memory only: a gateway restart drops a pending request and
// its dialog, and the agent hears nothing more about it.
package secretsapprove

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Op is what a request does.
type Op string

// The operations. Each mirrors a /secrets subcommand, except the agents_*
// trio, which edits allowed_agents / denied_agents.
const (
	OpSet         Op = "set"          // set (new or replace) a value; may also replace the section's hosts / agent rule
	OpRemove      Op = "remove"       // remove one secret
	OpHostsAdd    Op = "hosts_add"    // add one host to a section's allowed_hosts
	OpHostsRemove Op = "hosts_remove" // remove one host
	OpHostsClear  Op = "hosts_clear"  // remove the section's allowed_hosts
	OpBodyAdd     Op = "body_add"     // add one key to a section's allowed_in_body
	OpBodyRemove  Op = "body_remove"  // remove one key
	OpBodyClear   Op = "body_clear"   // remove the section's allowed_in_body
	OpAgentsAllow Op = "agents_allow" // allowed_agents = list (removes denied_agents)
	OpAgentsDeny  Op = "agents_deny"  // denied_agents = list (removes allowed_agents)
	OpAgentsClear Op = "agents_clear" // remove both lists: every agent may see the section
)

// Request is one validated change. Name is set for OpSet and OpRemove;
// Section for every op (for those two it is Name's section).
type Request struct {
	Op      Op
	Name    string   // section.key (set, remove)
	Section string   // the section the change touches
	Item    string   // the host (hosts_add/remove) or key (body_add/remove)
	Hosts   []string // set: the section's new allowed_hosts; nil = leave them
	Agents  []string // set: the section's new allowed_agents; agents_allow/deny: the list
}

// The bare TOML key shape secrets.toml is written with: renderState writes
// section and key names unquoted, so anything else would corrupt the file.
var (
	identRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	hostRe  = regexp.MustCompile(`^[a-z0-9:]([a-z0-9.:-]*[a-z0-9])?$`)
)

// metadataKeys are the per-section list settings; they are not secrets.
var metadataKeys = map[string]bool{
	"allowed_hosts": true, "allowed_agents": true, "denied_agents": true, "allowed_in_body": true,
}

// Usage is the tool's one-line grammar, used in parse errors.
const Usage = "set <section.key> [--allowed-hosts h1,h2] [--allowed-agents a1,a2] | remove <section.key> | " +
	"hosts <section> add|remove <host> | hosts <section> clear | body <section> add|remove <key> | body <section> clear | " +
	"agents <section> allow|deny <a1,a2> | agents <section> clear"

// Parse turns the tool's words into a Request. action is the first word;
// args the rest. allowedHosts and allowedAgents are the set-only flags,
// comma-separated; empty means not given.
func Parse(action string, args []string, allowedHosts, allowedAgents string) (Request, error) {
	if action != string(OpSet) && (allowedHosts != "" || allowedAgents != "") {
		return Request{}, errors.New("--allowed-hosts and --allowed-agents go only with set (use hosts / agents to change them on their own)")
	}
	switch action {
	case "set":
		if len(args) != 1 {
			return Request{}, fmt.Errorf("set takes one name: set <section.key> [--allowed-hosts h1,h2] [--allowed-agents a1,a2]. The value is never an argument: the user types it in the dialog")
		}
		section, err := checkName(args[0])
		if err != nil {
			return Request{}, err
		}
		r := Request{Op: OpSet, Name: args[0], Section: section}
		if allowedHosts != "" {
			if r.Hosts, err = parseHosts(allowedHosts); err != nil {
				return Request{}, err
			}
		}
		if allowedAgents != "" {
			if r.Agents, err = parseList("agent", allowedAgents); err != nil {
				return Request{}, err
			}
		}
		return r, nil
	case "remove":
		if len(args) != 1 {
			return Request{}, errors.New("usage: remove <section.key>")
		}
		section, err := checkName(args[0])
		if err != nil {
			return Request{}, err
		}
		return Request{Op: OpRemove, Name: args[0], Section: section}, nil
	case "hosts", "body", "agents":
		return parseSectionOp(action, args)
	case "":
		return Request{}, fmt.Errorf("no action: %s", Usage)
	default:
		return Request{}, fmt.Errorf("unknown action %q: %s", action, Usage)
	}
}

// parseSectionOp parses hosts|body|agents <section> <verb> [item].
func parseSectionOp(action string, args []string) (Request, error) {
	verbs := map[string]string{"hosts": "add|remove|clear", "body": "add|remove|clear", "agents": "allow|deny|clear"}[action]
	usage := fmt.Sprintf("usage: %s <section> %s …", action, verbs)
	if len(args) < 2 {
		return Request{}, errors.New(usage)
	}
	section, verb, rest := args[0], args[1], args[2:]
	if err := checkSection(section); err != nil {
		return Request{}, err
	}
	if verb == "clear" {
		if len(rest) != 0 {
			return Request{}, fmt.Errorf("%s <section> clear takes nothing more", action)
		}
		return Request{Op: Op(action + "_clear"), Section: section}, nil
	}
	switch action {
	case "hosts", "body":
		if verb != "add" && verb != "remove" {
			return Request{}, errors.New(usage)
		}
		if len(rest) != 1 {
			return Request{}, fmt.Errorf("%s <section> %s takes one item", action, verb)
		}
		item := rest[0]
		if action == "hosts" {
			hosts, err := parseHosts(item)
			if err != nil {
				return Request{}, err
			}
			if len(hosts) != 1 {
				return Request{}, errors.New("hosts add/remove takes one host")
			}
			item = hosts[0]
		} else if !identRe.MatchString(item) || metadataKeys[item] {
			return Request{}, fmt.Errorf("%q is not a secret key name (letters, digits, _ and -)", item)
		}
		return Request{Op: Op(action + "_" + verb), Section: section, Item: item}, nil
	default: // agents
		if verb != "allow" && verb != "deny" {
			return Request{}, errors.New(usage)
		}
		if len(rest) == 0 {
			return Request{}, fmt.Errorf("agents <section> %s needs a list of agent ids (to drop the rule, use agents <section> clear)", verb)
		}
		agents, err := parseList("agent", strings.Join(rest, ","))
		if err != nil {
			return Request{}, err
		}
		return Request{Op: Op("agents_" + verb), Section: section, Agents: agents}, nil
	}
}

// checkName validates section.key and returns the section.
func checkName(name string) (string, error) {
	section, key, ok := strings.Cut(name, ".")
	if !ok || !identRe.MatchString(key) {
		return "", fmt.Errorf("%q is not a secret name: use section.key (letters, digits, _ and -), e.g. hetzner.api_key", name)
	}
	if err := checkSection(section); err != nil {
		return "", err
	}
	if metadataKeys[key] {
		return "", fmt.Errorf("%s is a list setting, not a secret: use hosts, body or agents", key)
	}
	return section, nil
}

// checkSection validates a section name. "agents" is reserved: secrets.toml
// keeps per-agent tables under [agents.<id>.<section>].
func checkSection(section string) error {
	if !identRe.MatchString(section) {
		return fmt.Errorf("%q is not a section name (letters, digits, _ and -)", section)
	}
	if section == "agents" {
		return errors.New(`"agents" is reserved in secrets.toml for per-agent tables; choose another section name`)
	}
	return nil
}

// parseHosts splits a comma-separated host list, lowercased like
// AddAllowedHost does. Hosts are exact hostnames (no scheme, path, port or
// wildcard): CheckHostAllowed compares the URL's hostname for equality.
func parseHosts(s string) ([]string, error) {
	list, err := parseList("host", strings.ToLower(s))
	if err != nil {
		return nil, err
	}
	for _, h := range list {
		if !hostRe.MatchString(h) {
			return nil, fmt.Errorf("%q is not a hostname: give the bare host (e.g. api.hetzner.cloud), no scheme, path, port or wildcard", h)
		}
	}
	return list, nil
}

// parseList splits a comma-separated list, trimming blanks and dropping
// duplicates. what names an element in errors.
func parseList(what, s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		if what == "agent" && !identRe.MatchString(part) {
			return nil, fmt.Errorf("%q is not an agent id", part)
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty %s list", what)
	}
	return out, nil
}

// agentIDs returns the agent ids a request names, sorted, for the
// known-agent check.
func (r Request) agentIDs() []string {
	out := append([]string(nil), r.Agents...)
	sort.Strings(out)
	return out
}

// NeedsValue reports whether the user types a value in the dialog.
func (r Request) NeedsValue() bool { return r.Op == OpSet }

// String is the request in the tool's own words, for logs and results. It
// never holds a value: requests carry none.
func (r Request) String() string {
	switch r.Op {
	case OpSet:
		s := "set " + r.Name
		if r.Hosts != nil {
			s += " --allowed-hosts " + strings.Join(r.Hosts, ",")
		}
		if r.Agents != nil {
			s += " --allowed-agents " + strings.Join(r.Agents, ",")
		}
		return s
	case OpRemove:
		return "remove " + r.Name
	case OpHostsAdd, OpHostsRemove, OpBodyAdd, OpBodyRemove:
		action, verb, _ := strings.Cut(string(r.Op), "_")
		return fmt.Sprintf("%s %s %s %s", action, r.Section, verb, r.Item)
	case OpAgentsAllow, OpAgentsDeny:
		_, verb, _ := strings.Cut(string(r.Op), "_")
		return fmt.Sprintf("agents %s %s %s", r.Section, verb, strings.Join(r.Agents, ","))
	default: // the *_clear ops
		action, _, _ := strings.Cut(string(r.Op), "_")
		return fmt.Sprintf("%s %s clear", action, r.Section)
	}
}
