package secretsapprove

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Store is the root secret store the service edits (secrets.Store satisfies
// it). Every mutator re-reads the file first; Save persists.
type Store interface {
	Names() []string
	Get(name string) (string, bool)
	Set(name, value string) error
	Remove(name string) (bool, error)
	Save() error
	SectionAllowedHosts(section string) []string
	SetAllowedHosts(section string, hosts []string) error
	AddAllowedHost(section, host string) error
	RemoveAllowedHost(section, host string) (bool, error)
	SectionAllowedInBody(section string) []string
	AddAllowedInBody(section, key string) error
	RemoveAllowedInBody(section, key string) (bool, error)
	SetAllowedInBody(section string, keys []string) error
	SectionAllowedAgents(section string) []string
	SectionDeniedAgents(section string) []string
	SetAllowedAgents(section string, agents []string) error
	SetDeniedAgents(section string, agents []string) error
}

// MaxValueLen bounds a typed value, in bytes. Real keys are far shorter; the
// cap stops a paste accident from writing a megabyte into secrets.toml.
const MaxValueLen = 16 << 10

// CheckValue trims surrounding whitespace (a pasted key often carries a
// newline) and rejects what secrets.toml cannot hold safely: an empty value, a
// control character (the file writer quotes with Go's %q, whose \x escapes
// TOML cannot read), or one over MaxValueLen. The error never quotes the value.
func CheckValue(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", errors.New("the value is empty")
	case len(v) > MaxValueLen:
		return "", fmt.Errorf("the value is longer than %d bytes", MaxValueLen)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return "", errors.New("the value contains a control character (a line break or tab inside it?)")
	}
	return v, nil
}

// precheck reports a request that cannot succeed against the store as it is
// now, before the user is asked.
func precheck(st Store, r Request) error {
	switch r.Op {
	case OpRemove:
		if _, ok := st.Get(r.Name); !ok {
			return fmt.Errorf("no secret %s to remove", r.Name)
		}
	case OpHostsRemove:
		if !slices.ContainsFunc(st.SectionAllowedHosts(r.Section), func(h string) bool { return strings.EqualFold(h, r.Item) }) {
			return fmt.Errorf("%s is not in [%s] allowed_hosts", r.Item, r.Section)
		}
	case OpBodyRemove:
		if !slices.Contains(st.SectionAllowedInBody(r.Section), r.Item) {
			return fmt.Errorf("%s is not in [%s] allowed_in_body", r.Item, r.Section)
		}
	}
	return nil
}

// describe is the dialog's header and markdown body: who asks, what changes,
// and the section's rule before and after. Every name in it passed Parse's
// character checks, so none can carry markdown.
func describe(st Store, agentID string, r Request) (header, text string) {
	var b strings.Builder
	sec := r.Section
	switch r.Op {
	case OpSet:
		header = "Set secret"
		state := "new secret"
		if _, ok := st.Get(r.Name); ok {
			state = "replaces the current value"
		}
		fmt.Fprintf(&b, "**%s** asks to set the secret `%s` (%s).\n\n", agentID, r.Name, state)
		hosts := st.SectionAllowedHosts(sec)
		if r.Hosts != nil {
			fmt.Fprintf(&b, "- Allowed hosts for [%s]: %s (now: %s)\n", sec, codeList(r.Hosts), codeListOr(hosts, "none"))
		} else {
			fmt.Fprintf(&b, "- Allowed hosts for [%s]: %s (unchanged)\n", sec, codeListOr(hosts, "none — http_request cannot use it until hosts are added"))
		}
		now := agentRule(st, sec)
		if r.Agents != nil {
			fmt.Fprintf(&b, "- Agents that may use [%s]: only %s (now: %s)\n", sec, codeList(r.Agents), now)
		} else {
			fmt.Fprintf(&b, "- Agents that may use [%s]: %s (unchanged)\n", sec, now)
		}
		b.WriteString("\nApprove, then type the value. It goes straight to secrets.toml: the agent never sees it.")
	case OpRemove:
		header = "Remove secret"
		fmt.Fprintf(&b, "**%s** asks to remove the secret `%s`.", agentID, r.Name)
	case OpHostsAdd, OpHostsRemove, OpHostsClear:
		header = "Secret hosts"
		cur := st.SectionAllowedHosts(sec)
		fmt.Fprintf(&b, "**%s** asks to change the hosts that may receive [%s] secrets.\n\n", agentID, sec)
		fmt.Fprintf(&b, "- Now: %s\n- After: %s", codeListOr(cur, "none"), codeListOr(afterList(cur, r), "none"))
	case OpBodyAdd, OpBodyRemove, OpBodyClear:
		header = "Secret body keys"
		cur := st.SectionAllowedInBody(sec)
		fmt.Fprintf(&b, "**%s** asks to change which [%s] keys may go in a request body (allowed_in_body).\n\n", agentID, sec)
		fmt.Fprintf(&b, "- Now: %s\n- After: %s", codeListOr(cur, "none"), codeListOr(afterList(cur, r), "none"))
	default: // agents_*
		header = "Secret agents"
		fmt.Fprintf(&b, "**%s** asks to change which agents may use [%s] secrets.\n\n", agentID, sec)
		after := "every agent"
		switch r.Op {
		case OpAgentsAllow:
			after = "only " + codeList(r.Agents)
		case OpAgentsDeny:
			after = "every agent except " + codeList(r.Agents)
		}
		fmt.Fprintf(&b, "- Now: %s\n- After: %s", agentRule(st, sec), after)
	}
	return header, b.String()
}

// afterList is a hosts/body list after r's change.
func afterList(cur []string, r Request) []string {
	switch r.Op {
	case OpHostsClear, OpBodyClear:
		return nil
	case OpHostsAdd, OpBodyAdd:
		if slices.ContainsFunc(cur, func(s string) bool { return strings.EqualFold(s, r.Item) }) {
			return cur
		}
		return append(slices.Clone(cur), r.Item)
	default: // remove
		return slices.DeleteFunc(slices.Clone(cur), func(s string) bool { return strings.EqualFold(s, r.Item) })
	}
}

// agentRule describes a section's agent rule in words.
func agentRule(st Store, section string) string {
	if a := st.SectionAllowedAgents(section); a != nil {
		if len(a) == 0 {
			return "no agent"
		}
		return "only " + codeList(a)
	}
	if d := st.SectionDeniedAgents(section); len(d) > 0 {
		return "every agent except " + codeList(d)
	}
	return "every agent"
}

func codeList(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = "`" + x + "`"
	}
	return strings.Join(out, ", ")
}

func codeListOr(xs []string, none string) string {
	if len(xs) == 0 {
		return none
	}
	return codeList(xs)
}

// apply makes r's change and saves the file. value is the typed value for a
// set (already through CheckValue), empty otherwise. The result is the
// agent-facing sentence; it never holds the value.
func apply(st Store, r Request, value string) (string, error) {
	var msg string
	switch r.Op {
	case OpSet:
		if err := st.Set(r.Name, value); err != nil {
			return "", err
		}
		if r.Hosts != nil {
			if err := st.SetAllowedHosts(r.Section, r.Hosts); err != nil {
				return "", err
			}
		}
		if r.Agents != nil {
			if err := st.SetAllowedAgents(r.Section, r.Agents); err != nil {
				return "", err
			}
		}
		msg = fmt.Sprintf("Secret %s set. Use it as {{secret:%s}}. Allowed hosts for [%s]: %s.", r.Name, r.Name, r.Section,
			plainListOr(st.SectionAllowedHosts(r.Section), "none (http_request cannot send it until hosts are added)"))
	case OpRemove:
		found, err := st.Remove(r.Name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("no secret %s to remove (it went before the user approved)", r.Name)
		}
		msg = fmt.Sprintf("Secret %s removed.", r.Name)
	case OpHostsAdd:
		if err := st.AddAllowedHost(r.Section, r.Item); err != nil {
			return "", err
		}
	case OpHostsRemove:
		found, err := st.RemoveAllowedHost(r.Section, r.Item)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("%s is no longer in [%s] allowed_hosts", r.Item, r.Section)
		}
	case OpHostsClear:
		if err := st.SetAllowedHosts(r.Section, nil); err != nil {
			return "", err
		}
	case OpBodyAdd:
		if err := st.AddAllowedInBody(r.Section, r.Item); err != nil {
			return "", err
		}
	case OpBodyRemove:
		found, err := st.RemoveAllowedInBody(r.Section, r.Item)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("%s is no longer in [%s] allowed_in_body", r.Item, r.Section)
		}
	case OpBodyClear:
		if err := st.SetAllowedInBody(r.Section, nil); err != nil {
			return "", err
		}
	case OpAgentsAllow:
		if err := st.SetAllowedAgents(r.Section, r.Agents); err != nil {
			return "", err
		}
	case OpAgentsDeny:
		if err := st.SetDeniedAgents(r.Section, r.Agents); err != nil {
			return "", err
		}
	case OpAgentsClear:
		if err := st.SetAllowedAgents(r.Section, nil); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown op %q", r.Op)
	}
	if err := st.Save(); err != nil {
		return "", fmt.Errorf("save secrets.toml: %w", err)
	}
	if msg == "" {
		msg = fmt.Sprintf("Done: %s. [%s] now: allowed_hosts %s; allowed_in_body %s; agents: %s.", r, r.Section,
			plainListOr(st.SectionAllowedHosts(r.Section), "none"), plainListOr(st.SectionAllowedInBody(r.Section), "none"),
			strings.ReplaceAll(agentRule(st, r.Section), "`", ""))
	}
	return msg, nil
}

func plainListOr(xs []string, none string) string {
	if len(xs) == 0 {
		return none
	}
	return strings.Join(xs, ", ")
}

// List renders every secret name with its section's rules: allowed hosts,
// allowed_in_body keys and the agent rule. Never a value. It reads the root
// view: the global sections, not any agent's own [agents.<id>.*] tables.
func List(st Store) string {
	names := st.Names()
	if len(names) == 0 {
		return "No secrets configured."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d secrets (names and rules only, never values):\n", len(names))
	lastSection := ""
	for _, name := range names {
		section, key, _ := strings.Cut(name, ".")
		if section != lastSection {
			lastSection = section
			fmt.Fprintf(&b, "\n[%s]  hosts: %s | in body: %s | agents: %s\n", section,
				plainListOr(st.SectionAllowedHosts(section), "none"),
				plainListOr(st.SectionAllowedInBody(section), "none"),
				strings.ReplaceAll(agentRule(st, section), "`", ""))
		}
		fmt.Fprintf(&b, "  %s\n", key)
	}
	return strings.TrimRight(b.String(), "\n")
}
