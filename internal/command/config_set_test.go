package command

import (
	"strings"
	"testing"

	"foci/internal/config"
)

func testConfigSetDeps(setFn func(path string, target config.SetTarget, value string) (string, error)) ConfigSetDeps {
	if setFn == nil {
		setFn = func(path string, target config.SetTarget, value string) (string, error) {
			return "", nil
		}
	}
	return ConfigSetDeps{
		ConfigPath:      "/tmp/test-foci.toml",
		AgentID:         "test-agent",
		SectionsFn:      config.FieldSections,
		FieldsInSection: config.FieldsInSection,
		LookupFn:        config.LookupField,
		SetInFileFn:     setFn,
	}
}

// TestConfigSetWizardHappyPath walks the three-step wizard (section → key → value)
// and verifies: each step returns the expected prompt, the wizard terminates after
// the value step, SetInFileFn receives the correct SetTarget (section, key) and a
// TOML-quoted string value, and the confirmation includes a restart hint.
func TestConfigSetWizardHappyPath(t *testing.T) {
	var captured config.SetTarget
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return "16384", nil
	})

	w := newConfigSetWizard(deps)

	// Step 0: section
	resp, done := w.Handle("agent_loop")
	if done {
		t.Fatal("should not be done after section")
	}
	if !strings.Contains(resp, "max_output_tokens") {
		t.Errorf("expected key listing with 'max_output_tokens', got %q", resp)
	}

	// Step 1: key
	resp, done = w.Handle("max_output_tokens")
	if done {
		t.Fatal("should not be done after key")
	}
	if !strings.Contains(resp, "New value") {
		t.Errorf("expected value prompt, got %q", resp)
	}
	if !strings.Contains(resp, "Current: not set") {
		t.Errorf("expected 'Current: not set' (no EffectiveValueFn), got %q", resp)
	}
	if !strings.Contains(resp, "/stop") {
		t.Errorf("expected /stop cancel hint, got %q", resp)
	}

	// Step 2: value
	resp, done = w.Handle("32768")
	if !done {
		t.Fatal("should be done after value")
	}
	if !strings.Contains(resp, "Set agent_loop.max_output_tokens") {
		t.Errorf("expected confirmation, got %q", resp)
	}
	if !strings.Contains(resp, "Restart") {
		t.Errorf("expected restart hint, got %q", resp)
	}

	if captured.Section != "agent_loop" {
		t.Errorf("target.Section = %q", captured.Section)
	}
	if captured.Key != "max_output_tokens" {
		t.Errorf("target.Key = %q", captured.Key)
	}
	if capturedValue != "32768" {
		t.Errorf("value = %q", capturedValue)
	}
}

// TestConfigSetWizardShowsCurrentValue verifies the wizard displays the
// effective running value (which includes defaults) when prompting for a new
// value, so the user knows what they're replacing.
func TestConfigSetWizardShowsCurrentValue(t *testing.T) {
	deps := testConfigSetDeps(nil)
	deps.EffectiveValueFn = func(section, key string) string {
		if section == "sessions" && key == "compaction_threshold" {
			return "0.7"
		}
		return ""
	}

	w := newConfigSetWizard(deps)

	w.Handle("sessions")
	resp, done := w.Handle("compaction_threshold")
	if done {
		t.Fatal("should not be done after key")
	}
	if !strings.Contains(resp, "Current: 0.7") {
		t.Errorf("expected 'Current: 0.7', got %q", resp)
	}
}

// TestConfigSetWizardAgentSection verifies that selecting the "agent" section
// remaps to "agents" in the SetTarget and populates AgentID, so the TOML writer
// targets the per-agent [[agents]] block instead of a nonexistent [agent] table.
func TestConfigSetWizardAgentSection(t *testing.T) {
	var captured config.SetTarget
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})

	w := newConfigSetWizard(deps)

	w.Handle("agent")
	w.Handle("loop.max_output_tokens")
	w.Handle("32768")

	if captured.Section != "agents" {
		t.Errorf("target.Section = %q, want 'agents'", captured.Section)
	}
	if captured.AgentID != "test-agent" {
		t.Errorf("target.AgentID = %q, want 'test-agent'", captured.AgentID)
	}
}

// TestConfigSetWizardInvalidSection verifies that entering a nonexistent section
// name does not terminate the wizard, returns an "Unknown section" error with a
// list of valid sections, and allows the user to retry.
func TestConfigSetWizardInvalidSection(t *testing.T) {
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	resp, done := w.Handle("nonexistent")
	if done {
		t.Error("invalid section should not end wizard")
	}
	if !strings.Contains(resp, "Unknown section") {
		t.Errorf("expected unknown section error, got %q", resp)
	}
	if !strings.Contains(resp, "agent_loop") {
		t.Errorf("expected section listing, got %q", resp)
	}
}

// TestConfigSetWizardInvalidKey verifies that after selecting a valid section,
// entering a nonexistent key does not terminate the wizard and returns an
// "Unknown key" error so the user can retry with a valid key name.
func TestConfigSetWizardInvalidKey(t *testing.T) {
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	w.Handle("agent_loop")

	resp, done := w.Handle("nonexistent_key")
	if done {
		t.Error("invalid key should not end wizard")
	}
	if !strings.Contains(resp, "Unknown key") {
		t.Errorf("expected unknown key error, got %q", resp)
	}
}

// TestConfigSetWizardInvalidValue verifies type validation at the value step:
// entering a non-numeric string for an integer field (max_tool_loops) returns an
// "Invalid value" error with a "Try again" prompt, without terminating the wizard.
func TestConfigSetWizardInvalidValue(t *testing.T) {
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	w.Handle("agent_loop")
	w.Handle("max_tool_loops") // FieldInt

	resp, done := w.Handle("not-a-number")
	if done {
		t.Error("invalid value should not end wizard")
	}
	if !strings.Contains(resp, "Invalid value") {
		t.Errorf("expected invalid value error, got %q", resp)
	}
	if !strings.Contains(resp, "Try again") {
		t.Errorf("expected retry prompt, got %q", resp)
	}
}

// TestConfigSetDirect verifies the one-shot "section.key=value" syntax: parses
// the section and key from the dotted path, TOML-quotes the string value, calls
// SetInFileFn with the correct SetTarget, and returns a "Set section.key" confirmation.
func TestConfigSetDirect(t *testing.T) {
	var captured config.SetTarget
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return "", nil
	})

	resp, err := ConfigSetDirect(deps, "agent_loop.max_output_tokens=32768")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if !strings.Contains(resp, "Set agent_loop.max_output_tokens") {
		t.Errorf("response = %q", resp)
	}

	if captured.Section != "agent_loop" || captured.Key != "max_output_tokens" {
		t.Errorf("target = %+v", captured)
	}
	if capturedValue != "32768" {
		t.Errorf("value = %q", capturedValue)
	}
}

// TestConfigSetDirectAgent verifies that direct mode with "agent.key=value"
// remaps the section to "agents" and populates AgentID on the SetTarget,
// mirroring the wizard's agent section remapping behaviour.
func TestConfigSetDirectAgent(t *testing.T) {
	var captured config.SetTarget
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "agent.loop.max_tool_loops=50")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}

	if captured.Section != "agents" || captured.AgentID != "test-agent" {
		t.Errorf("target = %+v", captured)
	}
	if captured.Key != "loop.max_tool_loops" {
		t.Errorf("target.Key = %q", captured.Key)
	}
}

// TestConfigSetDirectUnknownField verifies that direct mode returns an error containing
// "unknown config field" when the section.key path ("nonexistent.field") does not correspond
// to any known config field, preventing writes to nonexistent configuration keys.
func TestConfigSetDirectUnknownField(t *testing.T) {
	deps := testConfigSetDeps(nil)

	_, err := ConfigSetDirect(deps, "nonexistent.field=value")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown config field") {
		t.Errorf("error = %q", err)
	}
}

// TestConfigSetDirectMissingEquals verifies that direct mode returns an error when the input
// string lacks an "=" separator (e.g. "agent_loop.max_tool_loops" without "=value"), ensuring the parser
// rejects malformed input rather than silently misinterpreting it.
func TestConfigSetDirectMissingEquals(t *testing.T) {
	deps := testConfigSetDeps(nil)

	_, err := ConfigSetDirect(deps, "agent_loop.max_tool_loops")
	if err == nil {
		t.Fatal("expected error for missing =")
	}
}

// TestConfigSetDirectBool verifies that direct mode normalizes boolean-like input values
// (e.g. "yes") to their canonical TOML form ("true") before passing them to SetInFileFn,
// ensuring consistent boolean representation in the config file regardless of the user's
// input style.
func TestConfigSetDirectBool(t *testing.T) {
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		capturedValue = value
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "debug.messages_in_log=yes")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if capturedValue != "true" {
		t.Errorf("value = %q, want 'true'", capturedValue)
	}
}

// TestConfigSetDirectInt verifies that integer values pass through as bare
// numeric strings (not TOML-quoted), so the file contains `key = 50` rather
// than `key = "50"`.
func TestConfigSetDirectInt(t *testing.T) {
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		capturedValue = value
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "agent_loop.max_tool_loops=50")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if capturedValue != "50" {
		t.Errorf("value = %q, want '50'", capturedValue)
	}
}

// TestConfigSetDirectShowsOldValue verifies that when SetInFileFn returns a
// previous value (e.g. `"old-model"`), the confirmation message includes "was"
// and the old value, giving the user feedback about what was replaced.
func TestConfigSetDirectShowsOldValue(t *testing.T) {
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		return "16384", nil
	})

	resp, err := ConfigSetDirect(deps, "agent_loop.max_output_tokens=32768")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if !strings.Contains(resp, "was") {
		t.Errorf("expected old value in response, got %q", resp)
	}
	if !strings.Contains(resp, "16384") {
		t.Errorf("expected old value in response, got %q", resp)
	}
}

// TestConfigSetSectionKey verifies the "section key" form (from keyboard button
// selection) feeds both values into the wizard and returns the value prompt,
// skipping the section listing and key listing steps.
func TestConfigSetSectionKey(t *testing.T) {
	registry := NewRegistry()
	deps := testConfigSetDeps(nil)
	deps.Registry = registry

	text, err := configSet(&deps, "agent_loop max_output_tokens")
	if err != nil {
		t.Fatalf("configSet: %v", err)
	}
	if !strings.Contains(text, "New value") {
		t.Errorf("expected value prompt, got %q", text)
	}
	if !strings.Contains(text, "agent_loop") {
		t.Errorf("expected section in prompt, got %q", text)
	}
}

// TestConfigSetSectionKeyValue verifies the "section key value" form (from
// boolean keyboard button) performs a direct set without starting a wizard.
func TestConfigSetSectionKeyValue(t *testing.T) {
	var captured config.SetTarget
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return "", nil
	})

	text, err := configSet(&deps, "debug messages_in_log true")
	if err != nil {
		t.Fatalf("configSet: %v", err)
	}
	if !strings.Contains(text, "Set debug.messages_in_log") {
		t.Errorf("expected confirmation, got %q", text)
	}
	if captured.Section != "debug" || captured.Key != "messages_in_log" {
		t.Errorf("target = %+v", captured)
	}
	if capturedValue != "true" {
		t.Errorf("value = %q, want 'true'", capturedValue)
	}
}
