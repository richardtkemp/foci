package command

import (
	"context"
	"fmt"
	"sort"
	"strings"
)
func NewModelCommand(getModel func(context.Context) string, setModel func(context.Context, string, string, string), resolveModel func(string) (string, string, string), modelAliases map[string]string) *Command {
	return &Command{
		Name:        "model",
		Description: "Show or switch model (supports endpoint:alias syntax, e.g. gemini:flash)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return fmt.Sprintf("Current model: %s", getModel(ctx)), nil
			}
			endpoint, resolved, format := resolveModel(args)
			setModel(ctx, endpoint, resolved, format)
			display := resolved
			if endpoint != "" {
				display = endpoint + ":" + resolved
			}
			return fmt.Sprintf("Model switched to: %s", display), nil
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			current := getModel(ctx)
			if len(modelAliases) > 0 {
				// Use aliases sorted alphabetically
				names := make([]string, 0, len(modelAliases))
				for alias := range modelAliases {
					names = append(names, alias)
				}
				sort.Strings(names)
				var opts []KeyboardOption
				for _, alias := range names {
					label := alias
					// Match against the alias value (which includes endpoint prefix)
					// Current model is just the model ID, so check if alias value ends with it
					aliasVal := modelAliases[alias]
					if aliasVal == current || (strings.Contains(aliasVal, ":") && strings.HasSuffix(aliasVal, ":"+current)) {
						label += " ✓"
					}
					opts = append(opts, KeyboardOption{Label: label, Data: alias})
				}
				return opts
			}
			// Fallback: show common model names
			models := []string{"haiku", "sonnet", "opus"}
			var opts []KeyboardOption
			for _, m := range models {
				label := m
				if strings.Contains(current, m) {
					label += " ✓"
				}
				opts = append(opts, KeyboardOption{Label: label, Data: m})
			}
			return opts
		},
	}
}

// NewEffortCommand returns a /effort command to show or set the effort level.
// getEffort returns current effort; setEffort changes it (runtime only).
// Callbacks receive the command's context so callers can resolve per-session state.
func NewEffortCommand(getEffort func(context.Context) string, setEffort func(context.Context, string)) *Command {
	return &Command{
		Name:        "effort",
		Description: "Show or set effort level (low/medium/high)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			const optionsLine = "Options: 1) low  2) medium  3) high"
			if args == "" {
				e := getEffort(ctx)
				if e == "" {
					return "Effort: not set (using API default)\n" + optionsLine, nil
				}
				return fmt.Sprintf("Effort: %s\n%s", e, optionsLine), nil
			}
			arg := strings.ToLower(strings.TrimSpace(args))
			// Accept numeric aliases
			switch arg {
			case "1":
				arg = "low"
			case "2":
				arg = "medium"
			case "3":
				arg = "high"
			}
			switch arg {
			case "low", "medium", "high":
				setEffort(ctx, arg)
				return fmt.Sprintf("Effort set to: %s", arg), nil
			case "none", "off", "":
				setEffort(ctx, "")
				return "Effort cleared (using API default)", nil
			default:
				return fmt.Sprintf("Invalid effort level: %q\n%s", args, optionsLine), nil
			}
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			current := getEffort(ctx)
			levels := []string{"low", "medium", "high"}
			opts := make([]KeyboardOption, len(levels))
			for i, l := range levels {
				label := l
				if l == current {
					label += " ✓"
				}
				opts[i] = KeyboardOption{Label: label, Data: l}
			}
			return opts
		},
	}
}

// NewThinkingCommand returns a /thinking command to show or set the thinking mode.
// getThinking returns current mode; setThinking changes it (runtime only).
// Callbacks receive the command's context so callers can resolve per-session state.
func NewThinkingCommand(getThinking func(context.Context) string, setThinking func(context.Context, string)) *Command {
	return &Command{
		Name:        "thinking",
		Description: "Show or set thinking mode (off/adaptive)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			const optionsLine = "Options: 0) off  1) adaptive"
			if args == "" {
				t := getThinking(ctx)
				if t == "" || t == "off" {
					return "Thinking: off\n" + optionsLine, nil
				}
				return fmt.Sprintf("Thinking: %s\n%s", t, optionsLine), nil
			}
			arg := strings.ToLower(strings.TrimSpace(args))
			switch arg {
			case "0":
				arg = "off"
			case "1":
				arg = "adaptive"
			}
			switch arg {
			case "off", "none":
				setThinking(ctx, "off")
				return "Thinking: off", nil
			case "adaptive":
				setThinking(ctx, "adaptive")
				return "Thinking: adaptive", nil
			default:
				return fmt.Sprintf("Invalid thinking mode: %q\n%s", args, optionsLine), nil
			}
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			current := getThinking(ctx)
			opts := []KeyboardOption{
				{Label: "off", Data: "off"},
				{Label: "adaptive", Data: "adaptive"},
			}
			for i := range opts {
				if opts[i].Data == current || (current == "" && opts[i].Data == "off") {
					opts[i].Label += " ✓"
				}
			}
			return opts
		},
	}
}

