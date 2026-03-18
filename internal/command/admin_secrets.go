package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"foci/internal/display"
)

// SecretsStore is the interface for managing secrets.
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

// SecretsDeps holds dependencies for the /secrets wizard flows.
type SecretsDeps struct {
	Registry *Registry
	Store    SecretsStore
}

// secretSections returns unique sorted section names from the store's keys.
func secretSections(store SecretsStore) []string {
	seen := make(map[string]struct{})
	for _, name := range store.Names() {
		sec, _, _ := strings.Cut(name, ".")
		seen[sec] = struct{}{}
	}
	sections := make([]string, 0, len(seen))
	for s := range seen {
		sections = append(sections, s)
	}
	sort.Strings(sections)
	return sections
}

// secretKeysInSection returns keys within a section (without the section prefix).
func secretKeysInSection(store SecretsStore, section string) []string {
	var keys []string
	prefix := section + "."
	for _, name := range store.Names() {
		if strings.HasPrefix(name, prefix) {
			keys = append(keys, strings.TrimPrefix(name, prefix))
		}
	}
	sort.Strings(keys)
	return keys
}

// SecretsCommand creates the /secrets slash command for managing secrets.
func SecretsCommand() *Command {
	secretsExec := func(cc CommandContext, fn func(SecretsStore) (Response, error)) (Response, error) {
		store := secretsResolveStore(cc)
		if store == nil {
			return Response{Text: "Secrets store not configured."}, nil
		}
		return fn(store)
	}

	cmd := &Command{
		Name:        "secrets",
		Description: "Manage secrets (list/set/remove/hosts)",
		Category:    "operations",
		Subcommands: []Subcommand{
			{
				Name:        "list",
				Description: "List all secrets",
				Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
					return secretsExec(cc, func(store SecretsStore) (Response, error) {
						return Response{Text: secretsList(store)}, nil
					})
				},
			},
			{
				Name:        "set",
				Description: "Set a secret value",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					return secretsExec(cc, func(store SecretsStore) (Response, error) {
						return secretsSetDispatch(cc, store, strings.Fields(req.Args))
					})
				},
			},
			{
				Name:        "remove",
				Description: "Remove a secret",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					return secretsExec(cc, func(store SecretsStore) (Response, error) {
						return secretsRemoveDispatch(store, strings.Fields(req.Args))
					})
				},
			},
			{
				Name:        "hosts",
				Description: "Manage allowed hosts for a secret section",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					return secretsExec(cc, func(store SecretsStore) (Response, error) {
						return secretsHostsDispatch(cc, store, strings.Fields(req.Args))
					})
				},
			},
		},
		ChainKeyboard: func(_ context.Context, subcommand string, cc CommandContext) []KeyboardOption {
			store := secretsResolveStore(cc)
			if store == nil {
				return nil
			}
			return secretsChainKeyboard(store, subcommand)
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

// secretsResolveStore returns the store from SecretsDeps if available, falling back to SecretsStore.
func secretsResolveStore(cc CommandContext) SecretsStore {
	if cc.SecretsDeps != nil && cc.SecretsDeps.Store != nil {
		return cc.SecretsDeps.Store
	}
	return cc.SecretsStore
}

// secretsChainKeyboard provides drill-down keyboard navigation.
func secretsChainKeyboard(store SecretsStore, subcommand string) []KeyboardOption {
	parts := strings.Fields(subcommand)
	if len(parts) == 0 {
		return nil
	}

	switch parts[0] {
	case "set":
		return secretsChainSectionKeys(store, "set", parts[1:])
	case "remove":
		return secretsChainSectionKeys(store, "remove", parts[1:])
	case "hosts":
		return secretsChainHosts(store, parts[1:])
	default:
		return nil
	}
}

// secretsChainSectionKeys provides section → key drill-down for a given command prefix.
func secretsChainSectionKeys(store SecretsStore, prefix string, args []string) []KeyboardOption {
	switch len(args) {
	case 0: // section buttons
		sections := secretSections(store)
		opts := make([]KeyboardOption, len(sections))
		for i, s := range sections {
			opts[i] = KeyboardOption{Label: s, Data: prefix + " " + s}
		}
		return opts
	case 1: // key buttons within section
		keys := secretKeysInSection(store, args[0])
		if len(keys) == 0 {
			return nil
		}
		opts := make([]KeyboardOption, len(keys))
		for i, k := range keys {
			opts[i] = KeyboardOption{Label: k, Data: prefix + " " + args[0] + " " + k}
		}
		return opts
	default:
		return nil
	}
}

// secretsChainHosts: hosts → sections → [add, remove, clear] → (remove shows host buttons).
func secretsChainHosts(store SecretsStore, args []string) []KeyboardOption {
	switch len(args) {
	case 0: // "hosts" → section buttons
		sections := secretSections(store)
		opts := make([]KeyboardOption, len(sections))
		for i, s := range sections {
			opts[i] = KeyboardOption{Label: s, Data: "hosts " + s}
		}
		return opts
	case 1: // "hosts <section>" → action buttons
		return []KeyboardOption{
			{Label: "add", Data: "hosts " + args[0] + " add"},
			{Label: "remove", Data: "hosts " + args[0] + " remove"},
			{Label: "clear", Data: "hosts " + args[0] + " clear"},
		}
	case 2: // "hosts <section> remove" → host buttons
		if args[1] != "remove" {
			return nil
		}
		hosts := store.SectionAllowedHosts(args[0])
		if len(hosts) == 0 {
			return nil
		}
		opts := make([]KeyboardOption, len(hosts))
		for i, h := range hosts {
			opts[i] = KeyboardOption{Label: h, Data: "hosts " + args[0] + " remove " + h}
		}
		return opts
	default:
		return nil
	}
}

// secretsSetDispatch handles "set" subcommand routing including wizard activation.
func secretsSetDispatch(cc CommandContext, store SecretsStore, args []string) (Response, error) {
	switch len(args) {
	case 0:
		// Bare "set" — activate wizard if available.
		return secretsActivateSetWizard(cc, store, "", "")
	case 1:
		// "set <section.key>" — might be from direct input.
		if strings.Contains(args[0], ".") {
			return Response{Text: "Usage: /secrets set <section.key> <value>"}, nil
		}
		// "set <section>" — no key yet, shouldn't reach here from keyboard (chain handles it).
		return Response{Text: "Usage: /secrets set <section.key> <value>"}, nil
	case 2:
		if strings.Contains(args[0], ".") {
			// "set <section.key> <value>" — direct set.
			return secretsSetDirect(store, args)
		}
		// "set <section> <key>" — from keyboard chain. Activate wizard at value step.
		return secretsActivateSetWizard(cc, store, args[0], args[1])
	default:
		// "set <section.key> <value>" or "set <section> <key> <value>"
		if strings.Contains(args[0], ".") {
			return secretsSetDirect(store, args)
		}
		// "set <section> <key> <value...>" — assemble section.key and value.
		name := args[0] + "." + args[1]
		value := strings.Join(args[2:], " ")
		store.Set(name, value)
		if err := store.Save(); err != nil {
			return Response{}, fmt.Errorf("save secrets: %w", err)
		}
		return Response{Text: fmt.Sprintf("Secret %s set.", name)}, nil
	}
}

// secretsActivateSetWizard starts the set wizard, optionally pre-populating section and key.
func secretsActivateSetWizard(cc CommandContext, store SecretsStore, section, key string) (Response, error) {
	reg := secretsRegistry(cc)
	if reg == nil {
		// No registry — fall back to usage.
		return Response{Text: "Usage: /secrets set <section.key> <value>"}, nil
	}

	w := newSecretsSetWizard(store)
	reg.SetWizard(w)

	if section != "" && key != "" {
		// Fast-forward: section and key already known, skip to value prompt.
		w.section = section
		w.key = key
		w.step = 1

		hosts := store.SectionAllowedHosts(section)
		hostsStr := "(none)"
		if len(hosts) > 0 {
			hostsStr = strings.Join(hosts, ", ")
		}
		return Response{Text: fmt.Sprintf("Set value for %s.%s\nAllowed hosts for [%s]: %s\n\nEnter value (or /stop to cancel):", section, key, section, hostsStr)}, nil
	}

	// Bare — prompt for section.key.
	return Response{Text: "Enter secret name (section.key format, e.g. custom.api_key):"}, nil
}

// secretsRemoveDispatch handles "remove" subcommand routing.
func secretsRemoveDispatch(store SecretsStore, args []string) (Response, error) {
	switch len(args) {
	case 0:
		return Response{Text: "Usage: /secrets remove <section.key>"}, nil
	case 1:
		// "remove <section.key>" — direct remove.
		return secretsRemoveDirect(store, args[0])
	case 2:
		// "remove <section> <key>" — from keyboard chain. Assemble and remove.
		return secretsRemoveDirect(store, args[0]+"."+args[1])
	default:
		return Response{Text: "Usage: /secrets remove <section.key>"}, nil
	}
}

// secretsRemoveDirect removes a secret by full name (section.key).
func secretsRemoveDirect(store SecretsStore, name string) (Response, error) {
	if !store.Remove(name) {
		return Response{Text: fmt.Sprintf("Secret %s not found.", name)}, nil
	}
	if err := store.Save(); err != nil {
		return Response{}, fmt.Errorf("save secrets: %w", err)
	}
	return Response{Text: fmt.Sprintf("Secret %s removed.", name)}, nil
}

// secretsHostsDispatch handles "hosts" subcommand routing including wizard activation.
func secretsHostsDispatch(cc CommandContext, store SecretsStore, args []string) (Response, error) {
	if len(args) == 0 {
		return Response{Text: "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]"}, nil
	}

	section := args[0]

	if len(args) == 1 {
		hosts := store.SectionAllowedHosts(section)
		if len(hosts) == 0 {
			return Response{Text: fmt.Sprintf("[%s] allowed_hosts: (none)", section)}, nil
		}
		return Response{Text: fmt.Sprintf("[%s] allowed_hosts: %s", section, strings.Join(hosts, ", "))}, nil
	}

	action := args[1]
	switch action {
	case "add":
		if len(args) < 3 {
			// Activate hosts-add wizard.
			return secretsActivateHostsAddWizard(cc, store, section)
		}
		host := strings.ToLower(strings.TrimSpace(args[2]))
		store.AddAllowedHost(section, host)
		if err := store.Save(); err != nil {
			return Response{}, fmt.Errorf("save secrets: %w", err)
		}
		return Response{Text: fmt.Sprintf("Added %s to [%s] allowed_hosts.", host, section)}, nil

	case "remove":
		if len(args) < 3 {
			return Response{Text: "Usage: /secrets hosts <section> remove <host>"}, nil
		}
		host := args[2]
		if !store.RemoveAllowedHost(section, host) {
			return Response{Text: fmt.Sprintf("Host %s not found in [%s] allowed_hosts.", host, section)}, nil
		}
		if err := store.Save(); err != nil {
			return Response{}, fmt.Errorf("save secrets: %w", err)
		}
		return Response{Text: fmt.Sprintf("Removed %s from [%s] allowed_hosts.", host, section)}, nil

	case "clear":
		store.SetAllowedHosts(section, nil)
		if err := store.Save(); err != nil {
			return Response{}, fmt.Errorf("save secrets: %w", err)
		}
		return Response{Text: fmt.Sprintf("Cleared allowed_hosts for [%s].", section)}, nil

	default:
		return Response{Text: "Usage: /secrets hosts <section> [add <host> | remove <host> | clear]"}, nil
	}
}

// secretsActivateHostsAddWizard starts the hosts-add wizard for a section.
func secretsActivateHostsAddWizard(cc CommandContext, store SecretsStore, section string) (Response, error) {
	reg := secretsRegistry(cc)
	if reg == nil {
		return Response{Text: "Usage: /secrets hosts <section> add <host>"}, nil
	}

	w := newSecretsHostsAddWizard(store, section)
	reg.SetWizard(w)

	hosts := store.SectionAllowedHosts(section)
	current := "(none)"
	if len(hosts) > 0 {
		current = strings.Join(hosts, ", ")
	}
	return Response{Text: fmt.Sprintf("[%s] current allowed_hosts: %s\n\nEnter host to add (or /stop to cancel):", section, current)}, nil
}

// secretsRegistry returns the Registry from SecretsDeps if available.
func secretsRegistry(cc CommandContext) *Registry {
	if cc.SecretsDeps != nil {
		return cc.SecretsDeps.Registry
	}
	return nil
}

// secretsSetDirect handles direct "set section.key value" form.
func secretsSetDirect(store SecretsStore, args []string) (Response, error) {
	if len(args) < 2 {
		return Response{Text: "Usage: /secrets set <section.key> <value>"}, nil
	}
	name := args[0]
	if !strings.Contains(name, ".") {
		return Response{Text: "Key must be in section.key format (e.g. custom.api_key)"}, nil
	}
	value := strings.Join(args[1:], " ")
	store.Set(name, value)
	if err := store.Save(); err != nil {
		return Response{}, fmt.Errorf("save secrets: %w", err)
	}
	return Response{Text: fmt.Sprintf("Secret %s set.", name)}, nil
}

func secretsList(store SecretsStore) string {
	names := store.Names()
	if len(names) == 0 {
		return "No secrets configured."
	}
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
				sec = ""
				hosts = ""
			}
			tableRows = append(tableRows, []string{sec, k, hosts})
		}
	}
	return fmt.Sprintf("Secrets (%d keys)\n\n%s",
		len(names), display.MarkdownTable(cols, tableRows))
}
