package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"foci/internal/config"
)

// ModelCommand returns a /model command to show or switch the model.
func ModelCommand() *Command {
	return &Command{
		Name:        "model",
		Description: "Show or switch model (supports endpoint:alias syntax, e.g. gemini:flash)",
		Category:    "operations",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			if req.Args == "" {
				current := cc.Agent.SessionModel(req.SessionKey)
				return Response{Text: fmt.Sprintf("Current model: %s", current)}, nil
			}
			resolved, err := config.ResolveModel(req.Args, "", cc.ModelAliases)
			var endpoint, model, format string
			if err != nil {
				endpoint = ""
				model = req.Args
				format = ""
			} else {
				endpoint = resolved.Endpoint
				model = resolved.Developer + "/" + resolved.ModelID
				format = resolved.Format
			}
			var client interface{ Send(context.Context, *interface{}, interface{}) (interface{}, error) }
			_ = client // ResolveEndpointClient handled below
			if endpoint != "" && format != "" && cc.ClientProvider != nil {
				provClient := cc.ClientProvider.ResolveEndpointClient(endpoint, format)
				cc.Agent.SetSessionModel(req.SessionKey, model, endpoint, format, provClient)
			} else {
				cc.Agent.SetSessionModel(req.SessionKey, model, endpoint, format, nil)
			}
			display := model
			if endpoint != "" {
				display = endpoint + ":" + model
			}
			return Response{Text: fmt.Sprintf("Model switched to: %s", display)}, nil
		},
		KeyboardOptions: func(_ context.Context, cc CommandContext) []KeyboardOption {
			if len(cc.ModelAliases) > 0 {
				names := make([]string, 0, len(cc.ModelAliases))
				for alias := range cc.ModelAliases {
					names = append(names, alias)
				}
				sort.Strings(names)
				var opts []KeyboardOption
				for _, alias := range names {
					opts = append(opts, KeyboardOption{Label: alias, Data: alias})
				}
				return opts
			}
			models := []string{"haiku", "sonnet", "opus"}
			var opts []KeyboardOption
			for _, m := range models {
				opts = append(opts, KeyboardOption{Label: m, Data: m})
			}
			return opts
		},
	}
}

// EffortCommand returns a /effort command to show or set the effort level.
// Visible is set to hide the command when the current model doesn't support effort.
func EffortCommand() *Command {
	return &Command{
		Name:        "effort",
		Description: "Show or set effort level (low/medium/high)",
		Category:    "operations",
		Visible: func(_ context.Context, req Request, cc CommandContext) bool {
			return config.ModelCapabilities(cc.Agent.SessionModel(req.SessionKey)).Effort
		},
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			const optionsLine = "Options: 1) low  2) medium  3) high"
			if req.Args == "" {
				e := cc.Agent.SessionEffort(req.SessionKey)
				if e == "" {
					return Response{Text: "Effort: not set (using API default)\n" + optionsLine}, nil
				}
				return Response{Text: fmt.Sprintf("Effort: %s\n%s", e, optionsLine)}, nil
			}
			arg := strings.ToLower(strings.TrimSpace(req.Args))
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
				cc.Agent.SetSessionEffort(req.SessionKey, arg)
				return Response{Text: fmt.Sprintf("Effort set to: %s", arg)}, nil
			case "none", "off", "":
				cc.Agent.SetSessionEffort(req.SessionKey, "")
				return Response{Text: "Effort cleared (using API default)"}, nil
			default:
				return Response{Text: fmt.Sprintf("Invalid effort level: %q\n%s", req.Args, optionsLine)}, nil
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			levels := []string{"low", "medium", "high"}
			opts := make([]KeyboardOption, len(levels))
			for i, l := range levels {
				opts[i] = KeyboardOption{Label: l, Data: l}
			}
			return opts
		},
	}
}

// ThinkingCommand returns a /thinking command to show or set the thinking mode.
// Visible is set to hide the command when the current model doesn't support thinking.
func ThinkingCommand() *Command {
	return &Command{
		Name:        "thinking",
		Description: "Show or set thinking mode (off/adaptive)",
		Category:    "operations",
		Visible: func(_ context.Context, req Request, cc CommandContext) bool {
			return config.ModelCapabilities(cc.Agent.SessionModel(req.SessionKey)).Thinking
		},
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			const optionsLine = "Options: 0) off  1) adaptive"
			if req.Args == "" {
				t := cc.Agent.SessionThinking(req.SessionKey)
				if t == "" || t == "off" {
					return Response{Text: "Thinking: off\n" + optionsLine}, nil
				}
				return Response{Text: fmt.Sprintf("Thinking: %s\n%s", t, optionsLine)}, nil
			}
			arg := strings.ToLower(strings.TrimSpace(req.Args))
			switch arg {
			case "0":
				arg = "off"
			case "1":
				arg = "adaptive"
			}
			switch arg {
			case "off", "none":
				cc.Agent.SetSessionThinking(req.SessionKey, "off")
				return Response{Text: "Thinking: off"}, nil
			case "adaptive":
				cc.Agent.SetSessionThinking(req.SessionKey, "adaptive")
				return Response{Text: "Thinking: adaptive"}, nil
			default:
				return Response{Text: fmt.Sprintf("Invalid thinking mode: %q\n%s", req.Args, optionsLine)}, nil
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "off", Data: "off"},
				{Label: "adaptive", Data: "adaptive"},
			}
		},
	}
}

// SpeedCommand returns a /speed command to show or set Anthropic fast mode.
// Visible is set to hide the command when the current model doesn't support speed.
func SpeedCommand() *Command {
	return &Command{
		Name:        "speed",
		Description: "Show or set speed mode (standard/fast)",
		Category:    "operations",
		Visible: func(_ context.Context, req Request, cc CommandContext) bool {
			return config.ModelCapabilities(cc.Agent.SessionModel(req.SessionKey)).Speed
		},
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			const optionsLine = "Options: 0) standard  1) fast"

			// Gate: reject if current model doesn't support speed
			m := cc.Agent.SessionModel(req.SessionKey)
			if !config.ModelCapabilities(m).Speed {
				return Response{Text: fmt.Sprintf("Speed is not supported by %s (Opus only)", m)}, nil
			}

			if req.Args == "" {
				s := cc.Agent.SessionSpeed(req.SessionKey)
				if s == "" || s == "standard" {
					return Response{Text: "Speed: standard\n" + optionsLine}, nil
				}
				return Response{Text: fmt.Sprintf("Speed: %s\n%s", s, optionsLine)}, nil
			}
			arg := strings.ToLower(strings.TrimSpace(req.Args))
			switch arg {
			case "0":
				arg = "standard"
			case "1":
				arg = "fast"
			}
			switch arg {
			case "standard", "off", "none":
				cc.Agent.SetSessionSpeed(req.SessionKey, "")
				return Response{Text: "Speed: standard"}, nil
			case "fast":
				cc.Agent.SetSessionSpeed(req.SessionKey, "fast")
				return Response{Text: "Speed: fast (6x pricing, separate prompt cache)"}, nil
			default:
				return Response{Text: fmt.Sprintf("Invalid speed mode: %q\n%s", req.Args, optionsLine)}, nil
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "standard", Data: "standard"},
				{Label: "fast", Data: "fast"},
			}
		},
	}
}

// DisplayField represents one display setting with its effective value and override status.
type DisplayField struct {
	Key      string // config key name (e.g. "show_tool_calls")
	Value    string // effective value
	Override string // per-session override value (empty = using default)
}

// DisplayCommand returns a /display command to show or set per-session display overrides.
func DisplayCommand() *Command {
	return &Command{
		Name:        "display",
		Description: "Show or set display options (show_tool_calls, show_thinking, stream_output, display_width)",
		Category:    "operations",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			args := strings.TrimSpace(req.Args)

			if args == "reset" {
				cc.Agent.ClearSessionDisplayOverrides(req.SessionKey)
				return Response{Text: "Display overrides cleared — using config defaults."}, nil
			}

			if args == "" {
				return Response{Text: formatDisplayStatus(req.SessionKey, cc)}, nil
			}

			parts := strings.SplitN(args, " ", 2)
			key := strings.ToLower(parts[0])
			if len(parts) == 1 {
				return formatSingleDisplay(req.SessionKey, cc, key)
			}
			value := strings.TrimSpace(parts[1])
			return applyDisplaySetting(req.SessionKey, cc, key, value)
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
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

// displayFieldValue returns (override, effective) for a display field.
func displayFieldValue(sessionKey string, cc CommandContext, key string) (override, effective string) {
	switch key {
	case "show_tool_calls":
		override = cc.Agent.SessionShowToolCalls(sessionKey)
		if override != "" {
			return override, override
		}
		effective = "off"
		if cc.AgentConfig.ShowToolCalls != nil {
			effective = string(*cc.AgentConfig.ShowToolCalls)
		} else if cc.Config.Telegram.ShowToolCalls != nil {
			effective = string(*cc.Config.Telegram.ShowToolCalls)
		}
		return "", effective
	case "show_thinking":
		override = cc.Agent.SessionDisplayShowThinking(sessionKey)
		if override != "" {
			return override, override
		}
		effective = "off"
		if cc.AgentConfig.ShowThinking != nil {
			effective = string(*cc.AgentConfig.ShowThinking)
		} else if cc.Config.Telegram.ShowThinking != nil {
			effective = string(*cc.Config.Telegram.ShowThinking)
		}
		return "", effective
	case "stream_output":
		override = cc.Agent.SessionStreamOutput(sessionKey)
		if override != "" {
			eff := "off"
			if override == "true" {
				eff = "on"
			}
			return override, eff
		}
		effective = "off"
		if cc.Config.Telegram.StreamOutput {
			effective = "on"
		}
		return "", effective
	case "display_width":
		override = cc.Agent.SessionDisplayWidth(sessionKey)
		if override != "" {
			return override, override
		}
		effective = "44"
		if cc.AgentConfig.DisplayWidth != nil {
			effective = fmt.Sprintf("%d", *cc.AgentConfig.DisplayWidth)
		} else if cc.Config.Telegram.DisplayWidth != nil {
			effective = fmt.Sprintf("%d", *cc.Config.Telegram.DisplayWidth)
		}
		return "", effective
	}
	return "", ""
}

// allDisplayFields returns all display fields with their current status.
func allDisplayFields(sessionKey string, cc CommandContext) []DisplayField {
	keys := []string{"show_tool_calls", "show_thinking", "stream_output", "display_width"}
	fields := make([]DisplayField, len(keys))
	for i, key := range keys {
		override, effective := displayFieldValue(sessionKey, cc, key)
		fields[i] = DisplayField{Key: key, Value: effective, Override: override}
	}
	return fields
}

// formatDisplayStatus builds the full status string for all display settings.
func formatDisplayStatus(sessionKey string, cc CommandContext) string {
	var b strings.Builder
	b.WriteString("Display settings:\n")
	for _, field := range allDisplayFields(sessionKey, cc) {
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
func formatSingleDisplay(sessionKey string, cc CommandContext, key string) (Response, error) {
	if canonical, ok := displayKeyAliases[key]; ok {
		key = canonical
	}
	fields := allDisplayFields(sessionKey, cc)
	for _, f := range fields {
		if f.Key == key {
			if f.Override != "" {
				return Response{Text: fmt.Sprintf("%s: %s (override)", f.Key, f.Value)}, nil
			}
			return Response{Text: fmt.Sprintf("%s: %s", f.Key, f.Value)}, nil
		}
	}
	return Response{}, fmt.Errorf("unknown display key: %q\nValid keys: show_tool_calls, show_thinking, stream_output (stream), display_width (width)", key)
}

// applyDisplaySetting validates and applies a display setting override.
func applyDisplaySetting(sessionKey string, cc CommandContext, key, value string) (Response, error) {
	if canonical, ok := displayKeyAliases[key]; ok {
		key = canonical
	}
	value = strings.ToLower(value)

	switch key {
	case "show_tool_calls":
		switch value {
		case "off", "preview", "full":
			cc.Agent.SetSessionShowToolCalls(sessionKey, value)
			return Response{Text: fmt.Sprintf("show_tool_calls set to: %s", value)}, nil
		default:
			return Response{}, fmt.Errorf("invalid show_tool_calls value: %q\nOptions: off, preview, full", value)
		}

	case "show_thinking":
		switch value {
		case "off", "compact", "true":
			cc.Agent.SetSessionDisplayShowThinking(sessionKey, value)
			return Response{Text: fmt.Sprintf("show_thinking set to: %s", value)}, nil
		default:
			return Response{}, fmt.Errorf("invalid show_thinking value: %q\nOptions: off, compact, true", value)
		}

	case "stream_output":
		switch value {
		case "on", "true":
			cc.Agent.SetSessionStreamOutput(sessionKey, "true")
			return Response{Text: "stream_output set to: on"}, nil
		case "off", "false":
			cc.Agent.SetSessionStreamOutput(sessionKey, "false")
			return Response{Text: "stream_output set to: off"}, nil
		default:
			return Response{}, fmt.Errorf("invalid stream_output value: %q\nOptions: on, off", value)
		}

	case "display_width":
		w, err := strconv.Atoi(value)
		if err != nil || w < 20 || w > 200 {
			return Response{}, fmt.Errorf("invalid display_width: %q (must be 20–200)", value)
		}
		cc.Agent.SetSessionDisplayWidth(sessionKey, strconv.Itoa(w))
		return Response{Text: fmt.Sprintf("display_width set to: %d", w)}, nil

	default:
		return Response{}, fmt.Errorf("unknown display key: %q\nValid keys: show_tool_calls, show_thinking, stream_output, display_width", key)
	}
}
