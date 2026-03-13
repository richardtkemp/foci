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

func TestConfigSetWizardHappyPath(t *testing.T) {
	// Verifies the full wizard flow: section → key → value, confirming SetInFileFn is called correctly.
	var captured config.SetTarget
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return `"old-model"`, nil
	})

	w := newConfigSetWizard(deps)

	// Step 0: section
	resp, done := w.Handle("llm")
	if done {
		t.Fatal("should not be done after section")
	}
	if !strings.Contains(resp, "model") {
		t.Errorf("expected key listing with 'model', got %q", resp)
	}

	// Step 1: key
	resp, done = w.Handle("model")
	if done {
		t.Fatal("should not be done after key")
	}
	if !strings.Contains(resp, "New value") {
		t.Errorf("expected value prompt, got %q", resp)
	}

	// Step 2: value
	resp, done = w.Handle("new-model")
	if !done {
		t.Fatal("should be done after value")
	}
	if !strings.Contains(resp, "Set llm.model") {
		t.Errorf("expected confirmation, got %q", resp)
	}
	if !strings.Contains(resp, "Restart") {
		t.Errorf("expected restart hint, got %q", resp)
	}

	if captured.Section != "llm" {
		t.Errorf("target.Section = %q", captured.Section)
	}
	if captured.Key != "model" {
		t.Errorf("target.Key = %q", captured.Key)
	}
	if capturedValue != `"new-model"` {
		t.Errorf("value = %q", capturedValue)
	}
}

func TestConfigSetWizardAgentSection(t *testing.T) {
	// Verifies that the "agent" section targets the [[agents]] block with the correct agent ID.
	var captured config.SetTarget
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})

	w := newConfigSetWizard(deps)

	w.Handle("agent")
	w.Handle("model")
	w.Handle("opus")

	if captured.Section != "agents" {
		t.Errorf("target.Section = %q, want 'agents'", captured.Section)
	}
	if captured.AgentID != "test-agent" {
		t.Errorf("target.AgentID = %q, want 'test-agent'", captured.AgentID)
	}
}

func TestConfigSetWizardInvalidSection(t *testing.T) {
	// Verifies that an unknown section shows available sections.
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	resp, done := w.Handle("nonexistent")
	if done {
		t.Error("invalid section should not end wizard")
	}
	if !strings.Contains(resp, "Unknown section") {
		t.Errorf("expected unknown section error, got %q", resp)
	}
	if !strings.Contains(resp, "defaults") {
		t.Errorf("expected section listing, got %q", resp)
	}
}

func TestConfigSetWizardInvalidKey(t *testing.T) {
	// Verifies that an unknown key shows available keys for the section.
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	w.Handle("defaults")

	resp, done := w.Handle("nonexistent_key")
	if done {
		t.Error("invalid key should not end wizard")
	}
	if !strings.Contains(resp, "Unknown key") {
		t.Errorf("expected unknown key error, got %q", resp)
	}
}

func TestConfigSetWizardInvalidValue(t *testing.T) {
	// Verifies that an invalid value for the field type is rejected with a retry prompt.
	deps := testConfigSetDeps(nil)
	w := newConfigSetWizard(deps)

	w.Handle("defaults")
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

func TestConfigSetDirect(t *testing.T) {
	// Verifies direct mode parsing and execution.
	var captured config.SetTarget
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		capturedValue = value
		return "", nil
	})

	resp, err := ConfigSetDirect(deps, "llm.model=new-model")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if !strings.Contains(resp, "Set llm.model") {
		t.Errorf("response = %q", resp)
	}

	if captured.Section != "llm" || captured.Key != "model" {
		t.Errorf("target = %+v", captured)
	}
	if capturedValue != `"new-model"` {
		t.Errorf("value = %q", capturedValue)
	}
}

func TestConfigSetDirectAgent(t *testing.T) {
	// Verifies direct mode with agent section.
	var captured config.SetTarget
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		captured = target
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "agent.effort=high")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}

	if captured.Section != "agents" || captured.AgentID != "test-agent" {
		t.Errorf("target = %+v", captured)
	}
	if captured.Key != "effort" {
		t.Errorf("target.Key = %q", captured.Key)
	}
}

func TestConfigSetDirectUnknownField(t *testing.T) {
	// Verifies direct mode rejects unknown fields.
	deps := testConfigSetDeps(nil)

	_, err := ConfigSetDirect(deps, "nonexistent.field=value")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown config field") {
		t.Errorf("error = %q", err)
	}
}

func TestConfigSetDirectMissingEquals(t *testing.T) {
	// Verifies direct mode rejects missing equals sign.
	deps := testConfigSetDeps(nil)

	_, err := ConfigSetDirect(deps, "defaults.model")
	if err == nil {
		t.Fatal("expected error for missing =")
	}
}

func TestConfigSetDirectBool(t *testing.T) {
	// Verifies direct mode with boolean value normalization.
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		capturedValue = value
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "logging.messages_in_log=yes")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if capturedValue != "true" {
		t.Errorf("value = %q, want 'true'", capturedValue)
	}
}

func TestConfigSetDirectInt(t *testing.T) {
	// Verifies direct mode with integer value.
	var capturedValue string
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		capturedValue = value
		return "", nil
	})

	_, err := ConfigSetDirect(deps, "defaults.max_tool_loops=50")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if capturedValue != "50" {
		t.Errorf("value = %q, want '50'", capturedValue)
	}
}

func TestConfigSetDirectShowsOldValue(t *testing.T) {
	// Verifies the old value is shown in the result when replacing.
	deps := testConfigSetDeps(func(path string, target config.SetTarget, value string) (string, error) {
		return `"old-model"`, nil
	})

	resp, err := ConfigSetDirect(deps, "llm.model=new-model")
	if err != nil {
		t.Fatalf("ConfigSetDirect: %v", err)
	}
	if !strings.Contains(resp, "was") {
		t.Errorf("expected old value in response, got %q", resp)
	}
	if !strings.Contains(resp, `"old-model"`) {
		t.Errorf("expected old value quoted, got %q", resp)
	}
}
