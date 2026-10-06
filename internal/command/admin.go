package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/config"
	"foci/internal/display"
)
import

// ToolInfo holds data for a single tool in the /tools listing.
"foci/internal/log"

var (
	commandLog = log.NewComponentLogger("command")
	facetLog   = log.NewComponentLogger("facet")
)

type ToolInfo struct {
	Name        string
	Description string
}

// ToolsCommand returns a /tools command listing registered tools.
func ToolsCommand() *Command {
	return &Command{
		Name:        "tools",
		Description: "List registered tools",
		Category:    "session",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.ToolsRegistry == nil {
				return Response{Text: "No tools registered."}, nil
			}
			allTools := cc.ToolsRegistry.All()
			if len(allTools) == 0 {
				return Response{Text: "No tools registered."}, nil
			}
			cols := []display.Column{
				{Header: "Name"},
				{Header: "Description"},
			}
			tableRows := make([][]string, len(allTools))
			for i, t := range allTools {
				tableRows[i] = []string{t.Name, t.Description}
			}
			return Response{Text: display.MarkdownTable(cols, tableRows)}, nil
		},
	}
}

// ConfigCommand returns a /config command for viewing and editing the running config.
func ConfigCommand() *Command {
	cmd := &Command{
		Name:        "config",
		Description: "Show or edit config. Subcommands: toml, table, available, get, set",
		Category:    "diagnostics",
		Subcommands: []Subcommand{
			{
				Name:        "toml",
				Description: "Raw TOML of running config (secrets redacted)",
				Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
					return Response{Text: config.FormatConfigTOML(cc.Config, cc.AgentConfig)}, nil
				},
			},
			{
				Name:        "table",
				Description: "Formatted table of current config values",
				Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
					return Response{Parts: config.FormatConfigGrouped(cc.Config, cc.AgentConfig)}, nil
				},
			},
			{
				Name:        "available",
				Description: "Unset options with defaults",
				Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
					return Response{Text: config.FormatAvailable(cc.Config, cc.AgentConfig)}, nil
				},
			},
			{
				Name:        "get",
				Description: "Read an agent's value: get <agent> <key> (get <agent> lists live-settable keys)",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					if cc.ConfigSetDeps == nil {
						return Response{Text: "Config get is not available."}, nil
					}
					text, err := configGet(cc.ConfigSetDeps, req.Args)
					return Response{Text: text}, err
				},
			},
			{
				Name:        "set",
				Description: "Edit config file: section.key=value, or <agent> <key>=<value> for a named agent's live settings",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					if cc.ConfigSetDeps == nil {
						return Response{Text: "Config set is not available."}, nil
					}
					text, err := configSet(cc.ConfigSetDeps, req.SessionKey, req.Args)
					return Response{Text: text}, err
				},
			},
		},
		ChainKeyboard: func(_ context.Context, subcommand string, cc CommandContext) []KeyboardOption {
			if cc.ConfigSetDeps == nil {
				return nil
			}
			parts := strings.Fields(subcommand)
			if len(parts) == 0 || parts[0] != "set" {
				return nil
			}
			switch len(parts) {
			case 1: // "set" → section buttons
				sections := cc.ConfigSetDeps.SectionsFn()
				opts := make([]KeyboardOption, len(sections))
				for i, s := range sections {
					opts[i] = KeyboardOption{Label: s, Data: "set " + s}
				}
				return opts
			case 2: // "set <section>" → key buttons
				fields := cc.ConfigSetDeps.FieldsInSection(parts[1])
				if len(fields) == 0 {
					return nil
				}
				opts := make([]KeyboardOption, len(fields))
				for i, f := range fields {
					opts[i] = KeyboardOption{Label: f.Key, Data: "set " + parts[1] + " " + f.Key}
				}
				return opts
			case 3: // "set <section> <key>" → bool fields get true/false buttons
				field, ok := cc.ConfigSetDeps.LookupFn(parts[1] + "." + parts[2])
				if !ok || field.Type != config.FieldBool {
					return nil
				}
				return []KeyboardOption{
					{Label: "true", Data: "set " + parts[1] + " " + parts[2] + " true"},
					{Label: "false", Data: "set " + parts[1] + " " + parts[2] + " false"},
				}
			default:
				return nil
			}
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

// configSet handles /config set — either starts a wizard (bare) or does a direct
// set. scope is the requesting session's key (the wizard is scoped to it). The
// keyboard fast-forwards route through Registry.HandleMessage rather than
// calling the wizard directly, so completion clears the wizard and every
// advance checkpoints persistence.
func configSet(deps *ConfigSetDeps, scope, args string) (string, error) {
	// "<agent> <key>=<value>" — a bare agent id names whose [[agents]]
	// block to write (live fields only; see config_agent.go). Checked
	// before the "=" branch so this shape never falls into
	// ConfigSetDirect's unknown-field path for its space-containing path.
	if agentID, rest, ok := splitAgentSetArgs(args); ok {
		return configSetAgent(deps, agentID, rest)
	}

	if args != "" && strings.Contains(args, "=") {
		return ConfigSetDirect(*deps, args)
	}

	parts := strings.Fields(args)

	// "section key value" → direct set (from boolean keyboard button).
	if len(parts) == 3 {
		return ConfigSetDirect(*deps, parts[0]+"."+parts[1]+"="+parts[2])
	}

	if deps.Registry == nil {
		return "Config set wizard is not available.", nil
	}

	w := newConfigSetWizard(*deps)
	deps.Registry.SetWizard(scope, w)

	// "section key" → skip to value prompt (from key keyboard button).
	if len(parts) == 2 {
		resp, _, _ := deps.Registry.HandleMessage(scope, parts[0])
		if !deps.Registry.WizardActive(scope) {
			return resp, nil
		}
		resp, _, _ = deps.Registry.HandleMessage(scope, parts[1])
		return resp, nil
	}

	// Single arg = section name.
	if args != "" {
		resp, _, _ := deps.Registry.HandleMessage(scope, args)
		return resp, nil
	}

	sections := deps.SectionsFn()
	return fmt.Sprintf("Which section?\n%s", strings.Join(sections, ", ")), nil
}

// HelpCommand returns a /help command. With no argument it lists every
// command visible to the caller; "/help <command>" shows that command's
// description, aliases and subcommands (#1551). Both read the registry, so
// each command's own registered text is the single source.
// registry is needed to enumerate commands; pass it after registration.
func HelpCommand(registry *Registry) *Command {
	return &Command{
		Name:        "help",
		Description: "List available commands; /help <command> for details",
		Category:    "session",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			fields := strings.Fields(req.Args)
			if len(fields) == 0 {
				return Response{Text: helpTable(ctx, registry, req, cc)}, nil
			}
			name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
			cmd := registry.Get(name)
			if cmd == nil {
				return Response{Text: fmt.Sprintf("Unknown command /%s.\n\n%s", name, helpTable(ctx, registry, req, cc))}, nil
			}
			return Response{Text: helpDetail(ctx, cmd, req, cc)}, nil
		},
	}
}

// helpTable renders the no-argument /help listing, grouped by category.
func helpTable(ctx context.Context, registry *Registry, req Request, cc CommandContext) string {
	type group struct {
		emoji string
		label string
	}
	categoryOrder := []string{"observability", "operations", "diagnostics", "session"}
	categoryMeta := map[string]group{
		"observability": {emoji: "📊", label: "Observability"},
		"operations":    {emoji: "⚙️", label: "Operations"},
		"diagnostics":   {emoji: "🔍", label: "Diagnostics"},
		"session":       {emoji: "💬", label: "Session"},
	}
	groups := make(map[string][]*Command)
	var other []*Command

	for _, cmd := range registry.All() {
		if !Available(ctx, cmd, req, cc) {
			continue
		}
		if cmd.Category != "" {
			groups[cmd.Category] = append(groups[cmd.Category], cmd)
		} else {
			other = append(other, cmd)
		}
	}

	cols := []display.Column{
		{Header: "Command"},
		{Header: "Description"},
	}
	var rows [][]string
	for _, cat := range categoryOrder {
		cmds := groups[cat]
		if len(cmds) == 0 {
			continue
		}
		meta := categoryMeta[cat]
		rows = append(rows, []string{fmt.Sprintf("**%s %s**", meta.emoji, meta.label), ""})
		for _, cmd := range cmds {
			rows = append(rows, []string{"/" + cmd.Name, cmd.Description})
		}
	}
	if len(other) > 0 {
		rows = append(rows, []string{"**📦 Other**", ""})
		for _, cmd := range other {
			rows = append(rows, []string{"/" + cmd.Name, cmd.Description})
		}
	}
	return display.MarkdownTable(cols, rows)
}

// helpDetail renders "/help <command>": the command's description, its
// aliases, a line saying so when it is unavailable to this caller (with the
// reason when the registry knows it), and a table of its subcommands. It
// describes any registered command, including ones the table hides: calling
// an existing command "Unknown" would be false. Subcommands marked Hidden are
// still listed (Hidden only keeps them off the keyboard; they dispatch when
// typed); ones whose Visible func says no are left out.
func helpDetail(ctx context.Context, cmd *Command, req Request, cc CommandContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**/%s**", cmd.Name)
	if cmd.Description != "" {
		fmt.Fprintf(&b, " — %s", cmd.Description)
	}
	if len(cmd.Aliases) > 0 {
		aliases := make([]string, len(cmd.Aliases))
		for i, a := range cmd.Aliases {
			aliases[i] = "/" + a
		}
		fmt.Fprintf(&b, "\nAliases: %s", strings.Join(aliases, ", "))
	}
	if msg := checkRequires(cmd, cc); msg != "" {
		fmt.Fprintf(&b, "\nNot available on this agent: %s.", msg)
	} else if cmd.Visible != nil && !cmd.Visible(ctx, req, cc) {
		b.WriteString("\nNot available on this agent in its current configuration (e.g. its model or backend).")
	}
	var rows [][]string
	for _, sub := range cmd.Subcommands {
		if sub.Visible != nil && !sub.Visible(ctx, cc) {
			continue
		}
		rows = append(rows, []string{sub.Name, sub.Description})
	}
	if len(rows) > 0 {
		b.WriteString("\n\n")
		b.WriteString(display.MarkdownTable([]display.Column{{Header: "Subcommand"}, {Header: "Description"}}, rows))
	}
	return b.String()
}
