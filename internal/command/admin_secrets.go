package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/display"
)

type SecretsStore interface {
	Names() []string
	Set(name, value string)
	Remove(name string) bool
	Save() error
	SectionAllowedHosts(section string) []string
	AddAllowedHost(section, host string)
	RemoveAllowedHost(section, host string) bool
	SetAllowedHosts(section string, hosts []string)
}

// NewSecretsCommand creates the /secrets slash command for managing secrets.
func NewSecretsCommand(store SecretsStore) *Command {
	return &Command{
		Name:        "secrets",
		Description: "Manage secrets (list/set/remove)",
		Category:    "operations",
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "set", Data: "set"},
				{Label: "remove", Data: "remove"},
			}
		},
		Execute: func(ctx context.Context, args string) (string, error) {
			parts := strings.Fields(args)
			if len(parts) == 0 {
				return secretsUsage, nil
			}

			switch parts[0] {
			case "list":
				return secretsList(store)
			case "hosts":
				return secretsHostsSubcmd(store, parts[1:])
			case "set":
				return secretsSet(store, parts[1:])
			case "remove":
				return secretsRemove(store, parts[1:])
			default:
				return secretsUsage, nil
			}
		},
	}
}

const secretsUsage = "Usage: /secrets list | /secrets set <section.key> <value> | /secrets remove <section.key> | /secrets hosts <section> [add|remove|clear] [host]"

func secretsList(store SecretsStore) (string, error) {
	names := store.Names()
	if len(names) == 0 {
		return "No secrets configured.", nil
	}
	// Group by section, preserving insertion order
	type secGroup struct {
		name string
		keys []string
	}
	var groups []secGroup
	groupIdx := make(map[string]int)
	for _, name := range names {
		p := strings.SplitN(name, ".", 2)
		sec := p[0]
		key := name
		if len(p) == 2 {
			key = p[1]
		}
		if idx, ok := groupIdx[sec]; ok {
			groups[idx].keys = append(groups[idx].keys, key)
		} else {
			groupIdx[sec] = len(groups)
			groups = append(groups, secGroup{name: sec, keys: []string{key}})
		}
	}

	// Build hosts display per section
	sectionHosts := make(map[string]string)
	for _, g := range groups {
		hosts := store.SectionAllowedHosts(g.name)
		if len(hosts) == 0 {
			sectionHosts[g.name] = "(none)"
		} else {
			sectionHosts[g.name] = strings.Join(hosts, ", ")
		}
	}

	cols := []display.Column{
		{Header: "Section"},
		{Header: "Key"},
		{Header: "Allowed Hosts"},
	}
	var tableRows [][]string
	for _, g := range groups {
		for i, k := range g.keys {
			sec := g.name
			hosts := sectionHosts[g.name]
			if i > 0 {
				sec = ""   // don't repeat section name
				hosts = "" // don't repeat hosts
			}
			tableRows = append(tableRows, []string{sec, k, hosts})
		}
	}
	return fmt.Sprintf("Secrets (%d keys)\n\n%s",
		len(names), display.MarkdownTable(cols, tableRows)), nil
}

func secretsSet(store SecretsStore, args []string) (string, error) {
	if len(args) < 2 {
		return "Usage: /secrets set <section.key> <value>", nil
	}
	name := args[0]
	if !strings.Contains(name, ".") {
		return "Key must be in section.key format (e.g. custom.api_key)", nil
	}
	value := strings.Join(args[1:], " ")
	store.Set(name, value)
	if err := store.Save(); err != nil {
		return "", fmt.Errorf("save secrets: %w", err)
	}
	return fmt.Sprintf("Secret %s set.", name), nil
}

func secretsRemove(store SecretsStore, args []string) (string, error) {
	if len(args) < 1 {
		return "Usage: /secrets remove <section.key>", nil
	}
	name := args[0]
	if !store.Remove(name) {
		return fmt.Sprintf("Secret %s not found.", name), nil
	}
	if err := store.Save(); err != nil {
		return "", fmt.Errorf("save secrets: %w", err)
	}
	return fmt.Sprintf("Secret %s removed.", name), nil
}

// secretsHostsSubcmd handles /secrets hosts <section> [add|remove|clear] [host].
func secretsHostsSubcmd(store SecretsStore, args []string) (string, error) {
	if len(args) == 0 {
		return "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]", nil
	}

	section := args[0]

	// /secrets hosts <section> — show current hosts
	if len(args) == 1 {
		hosts := store.SectionAllowedHosts(section)
		if len(hosts) == 0 {
			return fmt.Sprintf("[%s] allowed_hosts: (none)", section), nil
		}
		return fmt.Sprintf("[%s] allowed_hosts: %s", section, strings.Join(hosts, ", ")), nil
	}

	action := args[1]
	switch action {
	case "add":
		if len(args) < 3 {
			return "Usage: /secrets hosts <section> add <host>", nil
		}
		host := strings.ToLower(strings.TrimSpace(args[2]))
		store.AddAllowedHost(section, host)
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Added %s to [%s] allowed_hosts.", host, section), nil

	case "remove":
		if len(args) < 3 {
			return "Usage: /secrets hosts <section> remove <host>", nil
		}
		host := args[2]
		if !store.RemoveAllowedHost(section, host) {
			return fmt.Sprintf("Host %s not found in [%s] allowed_hosts.", host, section), nil
		}
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Removed %s from [%s] allowed_hosts.", host, section), nil

	case "clear":
		store.SetAllowedHosts(section, nil)
		if err := store.Save(); err != nil {
			return "", fmt.Errorf("save secrets: %w", err)
		}
		return fmt.Sprintf("Cleared allowed_hosts for [%s].", section), nil

	default:
		return "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]", nil
	}
}
