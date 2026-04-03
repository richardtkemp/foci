package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/config"
	"foci/internal/display"
)

// ToolInfo holds data for a single tool in the /tools listing.
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
		Description: "Show or edit config. Subcommands: toml, table, available, set",
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
				Name:        "set",
				Description: "Edit config file",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					if cc.ConfigSetDeps == nil {
						return Response{Text: "Config set is not available."}, nil
					}
					text, err := configSet(cc.ConfigSetDeps, req.Args)
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

// configSet handles /config set — either starts a wizard (bare) or does a direct set.
func configSet(deps *ConfigSetDeps, args string) (string, error) {
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
	deps.Registry.SetWizard(w)

	// "section key" → skip to value prompt (from key keyboard button).
	if len(parts) == 2 {
		resp, done := w.Handle(parts[0])
		if done {
			deps.Registry.ClearWizard()
			return resp, nil
		}
		resp, done = w.Handle(parts[1])
		if done {
			deps.Registry.ClearWizard()
		}
		return resp, nil
	}

	// Single arg = section name.
	if args != "" {
		resp, done := w.Handle(args)
		if done {
			deps.Registry.ClearWizard()
		}
		return resp, nil
	}

	sections := deps.SectionsFn()
	return fmt.Sprintf("Which section?\n%s", strings.Join(sections, ", ")), nil
}

// HelpCommand returns a /help command that lists all registered commands.
// registry is needed to enumerate commands; pass it after registration.
func HelpCommand(registry *Registry) *Command {
	return &Command{
		Name:        "help",
		Description: "List available commands",
		Category:    "session",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
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
				if cmd.Hidden || (cmd.Visible != nil && !cmd.Visible(ctx, req, cc)) || checkRequires(cmd, cc) != "" {
					continue
				}
				if cmd.Category != "" {
					groups[cmd.Category] = append(groups[cmd.Category], cmd)
				} else {
					other = append(other, cmd)
				}
			}

			var sb strings.Builder
			for _, cat := range categoryOrder {
				cmds := groups[cat]
				if len(cmds) == 0 {
					continue
				}
				meta := categoryMeta[cat]
				fmt.Fprintf(&sb, "%s %s\n", meta.emoji, meta.label)
				for _, cmd := range cmds {
					fmt.Fprintf(&sb, "  /%s — %s\n", cmd.Name, cmd.Description)
				}
				sb.WriteByte('\n')
			}
			if len(other) > 0 {
				sb.WriteString("📦 Other\n")
				for _, cmd := range other {
					fmt.Fprintf(&sb, "  /%s — %s\n", cmd.Name, cmd.Description)
				}
				sb.WriteByte('\n')
			}
			return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
		},
	}
}
