package command

import (
	"context"
	"strings"
	"testing"
)

func TestSecretsListTable(t *testing.T) {
	// Verifies secrets are displayed in table format with section grouping.
	store := &mockSecretsStore{
		data: map[string]string{
			"anthropic.setup_token":     "x",
			"telegram.clutch":     "x",
			"telegram.clutchling": "x",
			"telegram.scout":      "x",
			"brave.api_key":       "x",
		},
		allowedHosts: map[string][]string{
			"anthropic": {"api.anthropic.com"},
		},
	}

	cmd := NewSecretsCommand(store)
	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Header with count
	if !strings.Contains(result, "Secrets (5 keys)") {
		t.Errorf("missing header in:\n%s", result)
	}
	// Table headers
	if !strings.Contains(result, "Section") || !strings.Contains(result, "Key") || !strings.Contains(result, "Allowed Hosts") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	// Separator
	if !strings.Contains(result, "---") {
		t.Errorf("missing separator in:\n%s", result)
	}
	// Section grouping — "telegram" should appear once, not three times
	if strings.Count(result, "telegram") != 1 {
		t.Errorf("section 'telegram' should appear once (not repeated for each key):\n%s", result)
	}
	// All keys present
	for _, key := range []string{"token", "clutch", "clutchling", "scout", "api_key"} {
		if !strings.Contains(result, key) {
			t.Errorf("missing key %q in:\n%s", key, result)
		}
	}
	// Allowed hosts column
	if !strings.Contains(result, "api.anthropic.com") {
		t.Errorf("missing allowed host in:\n%s", result)
	}
	if !strings.Contains(result, "(none)") {
		t.Errorf("missing (none) for sections without allowed_hosts in:\n%s", result)
	}
}

func TestSecretsListEmpty(t *testing.T) {
	// Verifies appropriate message for no secrets.
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := NewSecretsCommand(store)
	result, _ := cmd.Execute(context.Background(), "list")
	if result != "No secrets configured." {
		t.Errorf("result = %q", result)
	}
}

func TestSecretsHostsView(t *testing.T) {
	// Verifies viewing allowed hosts for a section.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	// View hosts for a section
	result, err := cmd.Execute(context.Background(), "hosts myapi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "api.example.com") || !strings.Contains(result, "api.backup.com") {
		t.Errorf("expected hosts in output: %s", result)
	}

	// View hosts for section without hosts
	result, _ = cmd.Execute(context.Background(), "hosts legacy")
	if !strings.Contains(result, "(none)") {
		t.Errorf("expected (none) for section without hosts: %s", result)
	}
}

func TestSecretsHostsAdd(t *testing.T) {
	// Verifies adding an allowed host to a section.
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi add api.new.com")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Added") {
		t.Errorf("expected Added message: %s", result)
	}
	if !store.saved {
		t.Error("expected Save() to be called")
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.new.com" {
		t.Errorf("hosts = %v", hosts)
	}
}

func TestSecretsHostsRemove(t *testing.T) {
	// Verifies removing an allowed host from a section.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi remove api.example.com")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Removed") {
		t.Errorf("expected Removed message: %s", result)
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.backup.com" {
		t.Errorf("hosts after remove = %v", hosts)
	}

	// Remove nonexistent
	result, _ = cmd.Execute(context.Background(), "hosts myapi remove nonexistent.com")
	if !strings.Contains(result, "not found") {
		t.Errorf("expected not found message: %s", result)
	}
}

func TestSecretsHostsClear(t *testing.T) {
	// Verifies clearing all allowed hosts for a section.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi clear")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Cleared") {
		t.Errorf("expected Cleared message: %s", result)
	}
	if store.SectionAllowedHosts("myapi") != nil {
		t.Error("hosts should be nil after clear")
	}
}

func TestSecretsHostsUsage(t *testing.T) {
	// Verifies usage message for hosts subcommand.
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := NewSecretsCommand(store)

	// No args
	result, _ := cmd.Execute(context.Background(), "hosts")
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage: %s", result)
	}

	// Invalid action
	result, _ = cmd.Execute(context.Background(), "hosts myapi invalid")
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage for invalid action: %s", result)
	}
}
