package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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

// DisplayField represents one display setting with its effective value and override status.
type DisplayField struct {
	Key      string // config key name (e.g. "show_tool_calls")
	Value    string // effective value
	Override string // per-session override value (empty = using default)
}

// DisplayGetters provides callbacks for reading per-session display overrides.
type DisplayGetters struct {
	ShowToolCalls func(ctx context.Context) (override, effective string)
	ShowThinking  func(ctx context.Context) (override, effective string)
	StreamOutput  func(ctx context.Context) (override, effective string)
	DisplayWidth  func(ctx context.Context) (override, effective string)
}

// DisplaySetters provides callbacks for writing per-session display overrides.
type DisplaySetters struct {
	SetShowToolCalls func(ctx context.Context, value string)
	SetShowThinking  func(ctx context.Context, value string)
	SetStreamOutput  func(ctx context.Context, value string)
	SetDisplayWidth  func(ctx context.Context, value string)
	ResetAll         func(ctx context.Context)
}

// NewDisplayCommand returns a /display command to show or set per-session display overrides.
// Supported keys: show_tool_calls, show_thinking, stream_output, display_width.
func NewDisplayCommand(getters DisplayGetters, setters DisplaySetters) *Command {
	return &Command{
		Name:        "display",
		Description: "Show or set display options (show_tool_calls, show_thinking, stream_output, display_width)",
		Category:    "operations",
		Execute: func(ctx context.Context, args string) (string, error) {
			args = strings.TrimSpace(args)

			// /display reset — clear all overrides
			if args == "reset" {
				setters.ResetAll(ctx)
				return "Display overrides cleared — using config defaults.", nil
			}

			// /display — show all current values
			if args == "" {
				return formatDisplayStatus(ctx, getters), nil
			}

			// /display <key> [value] — get or set a single key
			parts := strings.SplitN(args, " ", 2)
			key := strings.ToLower(parts[0])
			if len(parts) == 1 {
				// Show single key
				return formatSingleDisplay(ctx, getters, key)
			}
			value := strings.TrimSpace(parts[1])
			return applyDisplaySetting(ctx, setters, key, value)
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "show_tool_calls", Data: "show_tool_calls"},
				{Label: "show_thinking", Data: "show_thinking"},
				{Label: "stream_output", Data: "stream_output"},
				{Label: "display_width", Data: "display_width"},
				{Label: "reset", Data: "reset"},
			}
		},
	}
}

// formatDisplayStatus builds the full status string for all display settings.
func formatDisplayStatus(ctx context.Context, g DisplayGetters) string {
	var b strings.Builder
	b.WriteString("Display settings:\n")
	for _, field := range allDisplayFields(ctx, g) {
		if field.Override != "" {
			fmt.Fprintf(&b, "  %s: %s (override)\n", field.Key, field.Value)
		} else {
			fmt.Fprintf(&b, "  %s: %s\n", field.Key, field.Value)
		}
	}
	b.WriteString("\nUse /display <key> <value> to set, /display reset to clear all overrides.")
	return b.String()
}

// displayKeyAliases maps short alias names to canonical display key names.
var displayKeyAliases = map[string]string{
	"stream": "stream_output",
	"width":  "display_width",
}

// formatSingleDisplay returns the status of a single display key.
func formatSingleDisplay(ctx context.Context, g DisplayGetters, key string) (string, error) {
	if canonical, ok := displayKeyAliases[key]; ok {
		key = canonical
	}
	fields := allDisplayFields(ctx, g)
	for _, f := range fields {
		if f.Key == key {
			if f.Override != "" {
				return fmt.Sprintf("%s: %s (override)", f.Key, f.Value), nil
			}
			return fmt.Sprintf("%s: %s", f.Key, f.Value), nil
		}
	}
	return "", fmt.Errorf("unknown display key: %q\nValid keys: show_tool_calls, show_thinking, stream_output (stream), display_width (width)", key)
}

// allDisplayFields returns all display fields with their current status.
func allDisplayFields(ctx context.Context, g DisplayGetters) []DisplayField {
	fields := make([]DisplayField, 4)
	override, effective := g.ShowToolCalls(ctx)
	fields[0] = DisplayField{Key: "show_tool_calls", Value: effective, Override: override}
	override, effective = g.ShowThinking(ctx)
	fields[1] = DisplayField{Key: "show_thinking", Value: effective, Override: override}
	override, effective = g.StreamOutput(ctx)
	fields[2] = DisplayField{Key: "stream_output", Value: effective, Override: override}
	override, effective = g.DisplayWidth(ctx)
	fields[3] = DisplayField{Key: "display_width", Value: effective, Override: override}
	return fields
}

// applyDisplaySetting validates and applies a display setting override.
func applyDisplaySetting(ctx context.Context, s DisplaySetters, key, value string) (string, error) {
	if canonical, ok := displayKeyAliases[key]; ok {
		key = canonical
	}
	value = strings.ToLower(value)

	switch key {
	case "show_tool_calls":
		switch value {
		case "off", "preview", "full":
			s.SetShowToolCalls(ctx, value)
			return fmt.Sprintf("show_tool_calls set to: %s", value), nil
		default:
			return "", fmt.Errorf("invalid show_tool_calls value: %q\nOptions: off, preview, full", value)
		}

	case "show_thinking":
		switch value {
		case "off", "compact", "true":
			s.SetShowThinking(ctx, value)
			return fmt.Sprintf("show_thinking set to: %s", value), nil
		default:
			return "", fmt.Errorf("invalid show_thinking value: %q\nOptions: off, compact, true", value)
		}

	case "stream_output":
		switch value {
		case "on", "true":
			s.SetStreamOutput(ctx, "true")
			return "stream_output set to: on", nil
		case "off", "false":
			s.SetStreamOutput(ctx, "false")
			return "stream_output set to: off", nil
		default:
			return "", fmt.Errorf("invalid stream_output value: %q\nOptions: on, off", value)
		}

	case "display_width":
		w, err := strconv.Atoi(value)
		if err != nil || w < 20 || w > 200 {
			return "", fmt.Errorf("invalid display_width: %q (must be 20–200)", value)
		}
		s.SetDisplayWidth(ctx, strconv.Itoa(w))
		return fmt.Sprintf("display_width set to: %d", w), nil

	default:
		return "", fmt.Errorf("unknown display key: %q\nValid keys: show_tool_calls, show_thinking, stream_output, display_width", key)
	}
}

