package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/tools"
)

// ModelCommand returns a /model command to show or switch the model.
func ModelCommand() *Command {
	return &Command{
		Name:        "model",
		Description: "Show or switch model (supports endpoint:alias syntax, e.g. gemini:flash)",
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			sk := tools.SessionKeyFromContext(ctx)
			if req.Args == "" {
				current := cc.Agent.SessionModel(sk)
				return Response{Text: fmt.Sprintf("Current model: %s", current)}, nil
			}
			resolved, err := config.ResolveModel(req.Args, "", nil)
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
			var provClient provider.Client
			if endpoint != "" && format != "" && cc.ClientProvider != nil {
				provClient = cc.ClientProvider.ResolveEndpointClient(endpoint, format)
			}
			// Use the orchestrator that updates metadata AND notifies the backend.
			if err := cc.Agent.SetModel(ctx, sk, model, endpoint, format, provClient, req.Args); err != nil {
				return Response{Text: fmt.Sprintf("Model metadata updated to %s but backend switch failed: %v", model, err)}, nil
			}
			display := model
			if endpoint != "" && resolved != nil && endpoint != resolved.Developer {
				display = endpoint + ":" + model
			}
			return Response{Text: fmt.Sprintf("Model switched to: %s", display)}, nil
		},
		KeyboardOptions: func(_ context.Context, cc CommandContext) []KeyboardOption {
			return nil
		},
	}
}

// settingChoice defines one valid option for a session setting command.
type settingChoice struct {
	Label    string   // keyboard button label and canonical name
	Aliases  []string // additional accepted inputs (e.g. numeric shortcuts, synonyms)
	SetValue string   // value passed to setter
	Response string   // response text returned on success
	Hidden   bool     // if true, not shown in keyboard options (but still accepted as input)
}

// sessionSettingDef configures a session setting command built by newSessionSettingCommand.
type sessionSettingDef struct {
	Name         string
	Description  string
	OptionsHint  string                                    // shown below current value (e.g. "Options: 1) low  2) medium  3) high")
	Capability   func(config.ModelCaps) bool               // model capability check for Visible (nil = always visible)
	ModelDefault func(config.ModelDefaults) string          // extract this setting from ModelDefaults (nil = no model default fallback)
	GateExecute  bool                                      // also reject in Execute when capability is false
	GateMsg      string                                    // rejection message format (%s = model name)
	EmptyShow    string                                    // display when effective value is "" (e.g. "not set")
	DefaultShow  string                                    // display when getter returns "" or matches this value (e.g. "off", "standard")
	InvalidName  string                                    // noun for error messages (e.g. "effort level", "thinking mode")
	Get          func(CommandContext, string) string
	Set          func(CommandContext, string, string)
	Choices      []settingChoice
}

// effectiveDisplay computes the display string and source annotation for a
// session setting's current value. Used by both Execute (no-args) and KeyboardHeader.
func effectiveDisplay(def *sessionSettingDef, cc CommandContext, sessionKey string) (display, source string) {
	current := def.Get(cc, sessionKey)
	display = current
	if current == "" && def.ModelDefault != nil && cc.Agent != nil && cc.Agent.ModelDefaultsFn != nil {
		md := cc.Agent.ModelDefaultsFn(cc.Agent.SessionModel(sessionKey))
		if v := def.ModelDefault(md); v != "" {
			display = v
			source = " (model default)"
		}
	}
	if display == "" {
		if def.EmptyShow != "" {
			display = def.EmptyShow
		} else {
			display = def.DefaultShow
		}
	} else if display == def.DefaultShow {
		display = def.DefaultShow
	}
	return display, source
}

// newSessionSettingCommand builds a Command from a sessionSettingDef, eliminating
// the boilerplate shared by /effort, /thinking, /speed, and similar commands.
func newSessionSettingCommand(def sessionSettingDef) *Command {
	// Build input→choice lookup for O(1) matching.
	choiceMap := make(map[string]*settingChoice, len(def.Choices)*2)
	for i := range def.Choices {
		c := &def.Choices[i]
		choiceMap[c.Label] = c
		for _, alias := range c.Aliases {
			choiceMap[alias] = c
		}
	}

	cmd := &Command{
		Name:        def.Name,
		Description: def.Description,
		Category:    "operations",
	}

	if def.Capability != nil {
		cmd.Visible = func(ctx context.Context, req Request, cc CommandContext) bool {
			model := cc.Agent.SessionModel(tools.SessionKeyFromContext(ctx))
			if def.Capability(config.ModelCapabilities(model)) {
				return true
			}
			// Also visible if model config explicitly configures this setting.
			if def.ModelDefault != nil && cc.Agent.ModelDefaultsFn != nil {
				return def.ModelDefault(cc.Agent.ModelDefaultsFn(model)) != ""
			}
			return false
		}
	}

	cmd.Execute = func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		// Gate: reject if current model doesn't support this setting.
		if def.GateExecute && def.Capability != nil {
			m := cc.Agent.SessionModel(sk)
			if !def.Capability(config.ModelCapabilities(m)) {
				return Response{Text: fmt.Sprintf(def.GateMsg, m)}, nil
			}
		}

		// No args: show effective value (session override → model default → empty).
		if req.Args == "" {
			display, source := effectiveDisplay(&def, cc, sk)
			title := strings.ToUpper(def.Name[:1]) + def.Name[1:]
			return Response{Text: fmt.Sprintf("%s: %s%s\n%s", title, display, source, def.OptionsHint)}, nil
		}

		// Normalize and match input.
		arg := strings.ToLower(strings.TrimSpace(req.Args))
		if c, ok := choiceMap[arg]; ok {
			def.Set(cc, sk, c.SetValue)
			return Response{Text: c.Response}, nil
		}

		return Response{Text: fmt.Sprintf("Invalid %s: %q\n%s", def.InvalidName, req.Args, def.OptionsHint)}, nil
	}

	cmd.KeyboardOptions = func(_ context.Context, _ CommandContext) []KeyboardOption {
		opts := make([]KeyboardOption, 0, len(def.Choices))
		for _, c := range def.Choices {
			if !c.Hidden {
				opts = append(opts, KeyboardOption{Label: c.Label, Data: c.Label})
			}
		}
		return opts
	}

	cmd.KeyboardHeader = func(ctx context.Context, req Request, cc CommandContext) string {
		display, source := effectiveDisplay(&def, cc, tools.SessionKeyFromContext(ctx))
		title := strings.ToUpper(def.Name[:1]) + def.Name[1:]
		return fmt.Sprintf("/%s — %s: %s%s", def.Name, title, display, source)
	}

	return cmd
}

// EffortCommand returns a /effort command to show or set the effort level.
func EffortCommand() *Command {
	return newSessionSettingCommand(sessionSettingDef{
		Name:         "effort",
		Description:  "Show or set effort level (low/medium/high)",
		OptionsHint:  "Options: 1) low  2) medium  3) high",
		Capability:   func(c config.ModelCaps) bool { return c.Effort },
		ModelDefault: func(md config.ModelDefaults) string { return md.Effort },
		EmptyShow:    "not set",
		InvalidName: "effort level",
		Get:         func(cc CommandContext, sk string) string { return cc.Agent.SessionEffort(sk) },
		Set:         func(cc CommandContext, sk, v string) { cc.Agent.SetSessionEffort(sk, v) },
		Choices: []settingChoice{
			{Label: "low", Aliases: []string{"1"}, SetValue: "low", Response: "Effort set to: low"},
			{Label: "medium", Aliases: []string{"2"}, SetValue: "medium", Response: "Effort set to: medium"},
			{Label: "high", Aliases: []string{"3"}, SetValue: "high", Response: "Effort set to: high"},
			{Label: "off", Aliases: []string{"0"}, SetValue: "off", Response: "Effort: off (overrides model default)", Hidden: true},
			{Label: "none", Aliases: []string{"clear", "reset", ""}, SetValue: "", Response: "Effort cleared (using model default)", Hidden: true},
		},
	})
}

// ThinkingCommand returns a /thinking command to show or set the thinking mode.
func ThinkingCommand() *Command {
	return newSessionSettingCommand(sessionSettingDef{
		Name:         "thinking",
		Description:  "Show or set thinking mode (off/adaptive)",
		OptionsHint:  "Options: 0) off  1) adaptive",
		Capability:   func(c config.ModelCaps) bool { return c.Thinking },
		ModelDefault: func(md config.ModelDefaults) string { return md.Thinking },
		DefaultShow:  "off",
		InvalidName: "thinking mode",
		Get:         func(cc CommandContext, sk string) string { return cc.Agent.SessionThinking(sk) },
		Set:         func(cc CommandContext, sk, v string) { cc.Agent.SetSessionThinking(sk, v) },
		Choices: []settingChoice{
			{Label: "off", Aliases: []string{"0", "none"}, SetValue: "off", Response: "Thinking: off"},
			{Label: "adaptive", Aliases: []string{"1"}, SetValue: "adaptive", Response: "Thinking: adaptive"},
		},
	})
}

// SpeedCommand returns a /speed command to show or set Anthropic fast mode.
func SpeedCommand() *Command {
	return newSessionSettingCommand(sessionSettingDef{
		Name:         "speed",
		Description:  "Show or set speed mode (standard/fast)",
		OptionsHint:  "Options: 0) standard  1) fast",
		Capability:   func(c config.ModelCaps) bool { return c.Speed },
		ModelDefault: func(md config.ModelDefaults) string { return md.Speed },
		GateExecute:  true,
		GateMsg:     "Speed is not supported by %s (Opus only)",
		DefaultShow: "standard",
		InvalidName: "speed mode",
		Get:         func(cc CommandContext, sk string) string { return cc.Agent.SessionSpeed(sk) },
		Set:         func(cc CommandContext, sk, v string) { cc.Agent.SetSessionSpeed(sk, v) },
		Choices: []settingChoice{
			{Label: "standard", Aliases: []string{"0", "off"}, SetValue: "standard", Response: "Speed: standard (overrides model default)"},
			{Label: "fast", Aliases: []string{"1"}, SetValue: "fast", Response: "Speed: fast (6x pricing, separate prompt cache)"},
			{Label: "none", Aliases: []string{"clear", "reset", "none"}, SetValue: "", Response: "Speed cleared (using model default)", Hidden: true},
		},
	})
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
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			sk := tools.SessionKeyFromContext(ctx)
			args := strings.TrimSpace(req.Args)

			if args == "reset" {
				cc.Agent.ClearSessionDisplayOverrides(sk)
				return Response{Text: "Display overrides cleared — using config defaults."}, nil
			}

			if args == "" {
				return Response{Text: formatDisplayStatus(sk, cc)}, nil
			}

			parts := strings.SplitN(args, " ", 2)
			key := strings.ToLower(parts[0])
			if len(parts) == 1 {
				return formatSingleDisplay(sk, cc, key)
			}
			value := strings.TrimSpace(parts[1])
			return applyDisplaySetting(sk, cc, key, value)
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
		ChainKeyboard: func(_ context.Context, subcommand string, _ CommandContext) []KeyboardOption {
			switch subcommand {
			case "show_tool_calls":
				return []KeyboardOption{
					{Label: "off", Data: "show_tool_calls off"},
					{Label: "preview", Data: "show_tool_calls preview"},
					{Label: "full", Data: "show_tool_calls full"},
				}
			case "show_thinking":
				return []KeyboardOption{
					{Label: "off", Data: "show_thinking off"},
					{Label: "compact", Data: "show_thinking compact"},
					{Label: "true", Data: "show_thinking true"},
				}
			case "stream_output":
				return []KeyboardOption{
					{Label: "off", Data: "stream_output off"},
					{Label: "on", Data: "stream_output on"},
				}
			default:
				return nil
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
		if cc.AgentConfig.Display.ShowToolCalls != nil {
			effective = string(*cc.AgentConfig.Display.ShowToolCalls)
		} else {
			// Check platforms for default
			for _, p := range cc.Config.Platforms {
				if p.Display.ShowToolCalls != nil {
					effective = string(*p.Display.ShowToolCalls)
					break
				}
			}
		}
		return "", effective
	case "show_thinking":
		override = cc.Agent.SessionDisplayShowThinking(sessionKey)
		if override != "" {
			return override, override
		}
		effective = "off"
		if cc.AgentConfig.Display.ShowThinking != nil {
			effective = string(*cc.AgentConfig.Display.ShowThinking)
		} else {
			for _, p := range cc.Config.Platforms {
				if p.Display.ShowThinking != nil {
					effective = string(*p.Display.ShowThinking)
					break
				}
			}
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
		// Check agent platform config for stream_output
		for _, p := range cc.AgentConfig.Platforms {
			if p.Display.StreamOutput != nil && *p.Display.StreamOutput {
				effective = "on"
				break
			}
		}
		return "", effective
	case "display_width":
		override = cc.Agent.SessionDisplayWidth(sessionKey)
		if override != "" {
			return override, override
		}
		effective = "44"
		if tg := cc.AgentConfig.Platform("telegram"); tg != nil && tg.Display.DisplayWidth != nil {
			effective = fmt.Sprintf("%d", *tg.Display.DisplayWidth)
		} else if gp := cc.Config.Platform("telegram"); gp != nil && gp.Display.DisplayWidth != nil {
			effective = fmt.Sprintf("%d", *gp.Display.DisplayWidth)
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

// OverridesCommand returns a /overrides command to list and manage per-session overrides.
func OverridesCommand() *Command {
	return &Command{
		Name:        "overrides",
		Description: "Show or manage per-session setting overrides",
		Category:    "operations",
		DefaultExecute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			return formatOverridesStatus(tools.SessionKeyFromContext(ctx), cc), nil
		},
		Subcommands: []Subcommand{
			{
				Name:        "reset",
				Description: "Clear all overrides for this session",
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					cc.Agent.ClearAllSessionOverrides(tools.SessionKeyFromContext(ctx))
					return Response{Text: "All session overrides cleared."}, nil
				},
			},
			{
				Name:        "delete",
				Description: "Clear a single override by key",
				Hidden:      true,
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					key := strings.ToLower(strings.TrimSpace(req.Args))
					if key == "" {
						return Response{Text: "Usage: /overrides delete <key>"}, nil
					}
					return deleteOverride(tools.SessionKeyFromContext(ctx), cc, key)
				},
			},
		},
	}
}

// overrideKeyMap maps user-facing key names to their sessionStringSetting.
var overrideKeyMap = map[string]struct {
	clearFn func(CommandContext, string)
}{
	"effort":                 {func(cc CommandContext, sk string) { cc.Agent.SetSessionEffort(sk, "") }},
	"thinking":               {func(cc CommandContext, sk string) { cc.Agent.SetSessionThinking(sk, "") }},
	"speed":                  {func(cc CommandContext, sk string) { cc.Agent.SetSessionSpeed(sk, "") }},
	"model":                  {func(cc CommandContext, sk string) { cc.Agent.SetSessionModel(sk, "", "", "", nil) }},
	"model_endpoint":         {func(cc CommandContext, sk string) { cc.Agent.SetSessionModel(sk, cc.Agent.SessionModel(sk), "", "", nil) }},
	"model_format":           {func(cc CommandContext, sk string) { cc.Agent.SetSessionModel(sk, cc.Agent.SessionModel(sk), "", "", nil) }},
	"show_tool_calls":        {func(cc CommandContext, sk string) { cc.Agent.SetSessionShowToolCalls(sk, "") }},
	"display_show_thinking":  {func(cc CommandContext, sk string) { cc.Agent.SetSessionDisplayShowThinking(sk, "") }},
	"stream_output":          {func(cc CommandContext, sk string) { cc.Agent.SetSessionStreamOutput(sk, "") }},
	"display_width":          {func(cc CommandContext, sk string) { cc.Agent.SetSessionDisplayWidth(sk, "") }},
	"no_compact":             {func(cc CommandContext, sk string) { cc.Agent.SetSessionNoCompact(sk, false) }},
}

// formatOverridesStatus builds a display of all current session overrides.
func formatOverridesStatus(sessionKey string, cc CommandContext) Response {
	overrides := cc.Agent.SessionOverrides(sessionKey)
	if len(overrides) == 0 {
		return Response{Text: "No session overrides set."}
	}

	var b strings.Builder
	b.WriteString("Session overrides:\n")

	// Show in a deterministic order matching allSessionStringSettings + no_compact.
	type entry struct{ key, val string }
	var entries []entry
	for _, key := range []string{
		"effort", "thinking", "speed",
		"model", "model_endpoint", "model_format",
		"show_tool_calls", "display_show_thinking",
		"stream_output", "display_width",
		"no_compact",
	} {
		if v, ok := overrides[key]; ok {
			entries = append(entries, entry{key, v})
		}
	}

	for _, e := range entries {
		fmt.Fprintf(&b, "  %s = %s\n", e.key, e.val)
	}
	b.WriteString("\nUse /overrides reset to clear all, /overrides delete <key> to clear one.")
	return Response{Text: b.String()}
}

// deleteOverride clears a single session override by key name.
func deleteOverride(sessionKey string, cc CommandContext, key string) (Response, error) {
	entry, ok := overrideKeyMap[key]
	if !ok {
		var valid []string
		for k := range overrideKeyMap {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return Response{Text: fmt.Sprintf("Unknown override key %q.\nValid keys: %s", key, strings.Join(valid, ", "))}, nil
	}
	entry.clearFn(cc, sessionKey)
	return Response{Text: fmt.Sprintf("Override %q cleared.", key)}, nil
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
		case "false":
			value = "off"
		case "medium":
			value = "preview"
		case "true":
			value = "full"
		}
		switch value {
		case "off", "preview", "full":
			cc.Agent.SetSessionShowToolCalls(sessionKey, value)
			return Response{Text: fmt.Sprintf("show_tool_calls set to: %s", value)}, nil
		default:
			return Response{}, fmt.Errorf("invalid show_tool_calls value: %q\nOptions: off/false, preview/medium, full/true", value)
		}

	case "show_thinking":
		switch value {
		case "false":
			value = "off"
		case "medium":
			value = "compact"
		case "full":
			value = "true"
		}
		switch value {
		case "off", "compact", "true":
			cc.Agent.SetSessionDisplayShowThinking(sessionKey, value)
			return Response{Text: fmt.Sprintf("show_thinking set to: %s", value)}, nil
		default:
			return Response{}, fmt.Errorf("invalid show_thinking value: %q\nOptions: off/false, compact/medium, true/full", value)
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
