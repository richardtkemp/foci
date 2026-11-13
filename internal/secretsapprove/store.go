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

// Warning is the line every secrets dialog shows in large red type above its
// body (Dick 2026-11-13, #3092: "This is an operation on secrets, something
// should be big and red to make it obvious").
const Warning = "⚠ SECRETS"

// describe is the dialog's header and markdown body: who asks, then the change
// as a table (Dick 2026-11-13, #3096). A request that changes something that
// exists shows Before and After columns, and when its table mixes changed and
// unchanged rows the changed ones are bold; the app fades the rest. A new
// section, or a request that changes nothing, shows one Value column with no
// label for what stays. A secret is two rows, its section as Group and its
// key as Name, spelled as stored (Dick 2026-11-13, #3092: "Group: Hetzner /
// Name: api-key, rather than Hetzner.api_key"). Every name in it passed
// Parse's character checks, so
// none can carry markdown or break a table cell.
func describe(st Store, agentID string, r Request) (header, text string) {
	var b strings.Builder
	sec := r.Section
	exists := sectionExists(st, sec)
	group := row{field: "Group", before: "`" + sec + "`", after: "`" + sec + "`"}
	if !exists {
		group.value = "`" + sec + "` (new)"
	}
	rows := []row{group}
	switch r.Op {
	case OpSet:
		header = "Set secret"
		_, key, _ := strings.Cut(r.Name, ".")
		name := row{field: "Name", before: "none", after: "`" + key + "`", value: "`" + key + "` (new)"}
		what := "set a new secret"
		rows = append(rows, name)
		if _, ok := st.Get(r.Name); ok {
			what = "replace a secret's value"
			rows[1].before = rows[1].after
			rows = append(rows, row{field: "Value", before: "current value", after: "new value"})
		}
		fmt.Fprintf(&b, "**%s** asks to %s.\n\n", agentID, what)
		cur := st.SectionAllowedHosts(sec)
		hosts := cur
		if r.Hosts != nil {
			hosts = r.Hosts
		}
		rows = append(rows, row{field: "Allowed hosts", before: codeListOr(cur, "none"), after: codeListOr(hosts, "none"),
			value: codeListOr(hosts, "none (http_request cannot use it)")})
		now := agentRule(st, sec)
		after := now
		if r.Agents != nil {
			after = "only " + codeList(r.Agents)
		}
		rows = append(rows, row{field: "Agents", before: now, after: after})
	case OpRemove:
		header = "Remove secret"
		_, key, _ := strings.Cut(r.Name, ".")
		fmt.Fprintf(&b, "**%s** asks to remove a secret.\n\n", agentID)
		rows = append(rows, row{field: "Name", before: "`" + key + "`", after: "removed"})
	case OpHostsAdd, OpHostsRemove, OpHostsClear:
		header = "Secret hosts"
		cur := st.SectionAllowedHosts(sec)
		fmt.Fprintf(&b, "**%s** asks to change which hosts may receive a group's secrets.\n\n", agentID)
		rows = append(rows, row{field: "Allowed hosts", before: codeListOr(cur, "none"), after: codeListOr(afterList(cur, r), "none")})
	case OpBodyAdd, OpBodyRemove, OpBodyClear:
		header = "Secret body keys"
		cur := st.SectionAllowedInBody(sec)
		fmt.Fprintf(&b, "**%s** asks to change which of a group's keys may go in a request body (allowed_in_body).\n\n", agentID)
		rows = append(rows, row{field: "Body keys", before: codeListOr(cur, "none"), after: codeListOr(afterList(cur, r), "none")})
	default: // agents_*
		header = "Secret agents"
		fmt.Fprintf(&b, "**%s** asks to change which agents may use a group's secrets.\n\n", agentID)
		after := "every agent"
		switch r.Op {
		case OpAgentsAllow:
			after = "only " + codeList(r.Agents)
		case OpAgentsDeny:
			after = "every agent except " + codeList(r.Agents)
		}
		rows = append(rows, row{field: "Agents", before: agentRule(st, sec), after: after})
	}
	writeTable(&b, rows, exists)
	return header, b.String()
}

// row is one field of a dialog table: its value before the request and after
// it. value, when set, is what a one-column table shows instead of after (a
// new section's "(new)" marks, the hosts rule's consequence).
type row struct{ field, before, after, value string }

func (r row) changed() bool { return r.before != r.after }

// writeTable writes rows as a markdown table. Before and After columns only
// when the section exists and some row changes; otherwise one Value column.
// In a two-column table that mixes changed and unchanged rows, every cell of
// a changed row is bold: the app's secrets dialog fades the rows that are
// not, so the change stands out (#3096).
func writeTable(b *strings.Builder, rows []row, exists bool) {
	nChanged := 0
	for _, r := range rows {
		if r.changed() {
			nChanged++
		}
	}
	if !exists || nChanged == 0 {
		b.WriteString("| Field | Value |\n|---|---|\n")
		for _, r := range rows {
			v := r.after
			if r.value != "" && !exists {
				v = r.value
			}
			fmt.Fprintf(b, "| %s | %s |\n", r.field, v)
		}
		return
	}
	mixed := nChanged < len(rows)
	b.WriteString("| Field | Before | After |\n|---|---|---|\n")
	for _, r := range rows {
		cells := []string{r.field, r.before, r.after}
		if mixed && r.changed() {
			for i, c := range cells {
				cells[i] = "**" + c + "**"
			}
		}
		fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | "))
	}
}

// sectionExists reports whether secrets.toml already has the section: a
// secret in it, or any of its rules.
func sectionExists(st Store, sec string) bool {
	for _, n := range st.Names() {
		if s, _, ok := strings.Cut(n, "."); ok && s == sec {
			return true
		}
	}
	return st.SectionAllowedHosts(sec) != nil || st.SectionAllowedInBody(sec) != nil ||
		st.SectionAllowedAgents(sec) != nil || st.SectionDeniedAgents(sec) != nil
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
