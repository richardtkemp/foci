package command

import (
	"fmt"
	"strings"

	"foci/internal/config"
)

// ConfigSetDeps holds dependencies for the /config set wizard and direct mode.
type ConfigSetDeps struct {
	ConfigPath      string
	AgentID         string // current agent's ID, for targeting [[agents]] block
	SectionsFn      func() []string
	FieldsInSection func(section string) []config.ConfigField
	LookupFn        func(sectionKey string) (config.ConfigField, bool)
	SetInFileFn     func(path string, target config.SetTarget, value string) (string, error)
}

// configSetWizard implements WizardHandler for interactive config editing.
// Steps: 0 = pick section, 1 = pick key, 2 = enter value.
type configSetWizard struct {
	step int
	deps ConfigSetDeps

	section string
	key     string
	field   config.ConfigField
	target  config.SetTarget
}

func newConfigSetWizard(deps ConfigSetDeps) *configSetWizard {
	return &configSetWizard{deps: deps}
}

// Handle processes a wizard step and returns the response.
func (w *configSetWizard) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)

	switch w.step {
	case 0:
		return w.handleSection(text)
	case 1:
		return w.handleKey(text)
	case 2:
		return w.handleValue(text)
	default:
		return "Unexpected state.", true
	}
}

func (w *configSetWizard) handleSection(text string) (string, bool) {
	if text == "" {
		return "Section cannot be empty. Try again:", false
	}

	fields := w.deps.FieldsInSection(text)
	if len(fields) == 0 {
		sections := w.deps.SectionsFn()
		return fmt.Sprintf("Unknown section %q. Available: %s", text, strings.Join(sections, ", ")), false
	}

	w.section = strings.ToLower(text)

	// Build the key listing.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Keys in [%s]:\n", w.section))
	for _, f := range fields {
		sb.WriteString(fmt.Sprintf("  %s — %s\n", f.Key, f.Description))
	}
	sb.WriteString("\nWhich key?")
	w.step = 1
	return sb.String(), false
}

func (w *configSetWizard) handleKey(text string) (string, bool) {
	if text == "" {
		return "Key cannot be empty. Try again:", false
	}

	// Look up as section.key.
	lookup := w.section + "." + text
	field, ok := w.deps.LookupFn(lookup)
	if !ok {
		fields := w.deps.FieldsInSection(w.section)
		var keys []string
		for _, f := range fields {
			keys = append(keys, f.Key)
		}
		return fmt.Sprintf("Unknown key %q in [%s]. Available: %s", text, w.section, strings.Join(keys, ", ")), false
	}

	w.key = field.Key
	w.field = field

	// Build the target.
	if w.section == "agent" {
		w.target = config.SetTarget{Section: "agents", AgentID: w.deps.AgentID, Key: w.key}
	} else {
		w.target = config.SetTarget{Section: w.section, Key: w.key}
	}

	typeHint := fieldTypeHint(field.Type)
	w.step = 2
	return fmt.Sprintf("[%s] %s (%s)\n%s\nNew value:", w.section, w.key, typeHint, field.Description), false
}

func (w *configSetWizard) handleValue(text string) (string, bool) {
	if text == "" {
		return "Value cannot be empty. Try again:", false
	}

	formatted, err := config.FormatTOMLValue(text, w.field.Type)
	if err != nil {
		return fmt.Sprintf("Invalid value: %s. Try again:", err), false
	}

	oldValue, err := w.deps.SetInFileFn(w.deps.ConfigPath, w.target, formatted)
	if err != nil {
		return fmt.Sprintf("Failed to set: %s", err), true
	}

	return formatSetResult(w.section, w.key, formatted, oldValue), true
}

// ConfigSetDirect handles the direct /config set section.key=value form.
func ConfigSetDirect(deps ConfigSetDeps, args string) (string, error) {
	// Parse section.key=value
	eqIdx := strings.Index(args, "=")
	if eqIdx < 0 {
		return "", fmt.Errorf("expected section.key=value format")
	}

	path := strings.TrimSpace(args[:eqIdx])
	rawValue := strings.TrimSpace(args[eqIdx+1:])

	if path == "" || rawValue == "" {
		return "", fmt.Errorf("expected section.key=value format")
	}

	// Split into section.key
	dotIdx := strings.Index(path, ".")
	if dotIdx < 0 {
		return "", fmt.Errorf("expected section.key format (e.g. defaults.model)")
	}

	section := path[:dotIdx]
	key := path[dotIdx+1:]

	// Handle dotted agent sub-keys like agent.keepalive.enabled → section=agent, key=keepalive.enabled
	fullLookup := path
	field, ok := deps.LookupFn(fullLookup)
	if !ok {
		// Maybe it's a dotted sub-key: try section + rest-of-path
		// e.g. "agent.keepalive.enabled" → lookup "agent.keepalive.enabled"
		// Already tried above with fullLookup. Try without:
		return "", fmt.Errorf("unknown config field %q. Use /config set (bare) for interactive mode", path)
	}

	formatted, err := config.FormatTOMLValue(rawValue, field.Type)
	if err != nil {
		return "", err
	}

	var target config.SetTarget
	if section == "agent" {
		target = config.SetTarget{Section: "agents", AgentID: deps.AgentID, Key: key}
	} else {
		target = config.SetTarget{Section: section, Key: key}
	}

	oldValue, err := deps.SetInFileFn(deps.ConfigPath, target, formatted)
	if err != nil {
		return "", err
	}

	return formatSetResult(section, key, formatted, oldValue), nil
}

func formatSetResult(section, key, formatted, oldValue string) string {
	var sb strings.Builder
	if oldValue != "" {
		fmt.Fprintf(&sb, "Set %s.%s = %s (was %s)", section, key, formatted, oldValue)
	} else {
		fmt.Fprintf(&sb, "Set %s.%s = %s", section, key, formatted)
	}
	sb.WriteString("\nRestart to take effect.")
	return sb.String()
}

func fieldTypeHint(ft config.FieldType) string {
	switch ft {
	case config.FieldString:
		return "string"
	case config.FieldInt:
		return "integer"
	case config.FieldFloat:
		return "float"
	case config.FieldBool:
		return "bool"
	case config.FieldDuration:
		return "duration, e.g. 5m, 30s, 1h"
	}
	return "value"
}
