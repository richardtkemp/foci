package command

import (
	"context"
	"strings"
	"testing"
)

// secretsCC returns a CommandContext with the given mock secrets store.
func secretsCC(store SecretsStore) CommandContext {
	return CommandContext{SecretsStore: store}
}

// TestSecretsListTable verifies secrets are displayed in table format with section grouping.
func TestSecretsListTable(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"anthropic.setup_token": "x",
			"telegram.clutch":      "x",
			"telegram.clutchling":  "x",
			"telegram.scout":       "x",
			"brave.api_key":        "x",
		},
		allowedHosts: map[string][]string{
			"anthropic": {"api.anthropic.com"},
		},
	}

	cmd := SecretsCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, secretsCC(store))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Header with count
	if !strings.Contains(result.Text, "Secrets (5 keys)") {
		t.Errorf("missing header in:\n%s", result.Text)
	}
	// Table headers
	if !strings.Contains(result.Text, "Section") || !strings.Contains(result.Text, "Key") || !strings.Contains(result.Text, "Allowed Hosts") {
		t.Errorf("missing table headers in:\n%s", result.Text)
	}
	// Separator
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator in:\n%s", result.Text)
	}
	// Section grouping — "telegram" should appear once, not three times
	if strings.Count(result.Text, "telegram") != 1 {
		t.Errorf("section 'telegram' should appear once (not repeated for each key):\n%s", result.Text)
	}
	// All keys present
	for _, key := range []string{"token", "clutch", "clutchling", "scout", "api_key"} {
		if !strings.Contains(result.Text, key) {
			t.Errorf("missing key %q in:\n%s", key, result.Text)
		}
	}
	// Allowed hosts column
	if !strings.Contains(result.Text, "api.anthropic.com") {
		t.Errorf("missing allowed host in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "(none)") {
		t.Errorf("missing (none) for sections without allowed_hosts in:\n%s", result.Text)
	}
}

// TestSecretsListEmpty verifies appropriate message for no secrets.
func TestSecretsListEmpty(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := SecretsCommand()
	result, _ := cmd.Execute(context.Background(), Request{Args: "list"}, secretsCC(store))
	if result.Text != "No secrets configured." {
		t.Errorf("result = %q", result.Text)
	}
}

// TestSecretsHostsView verifies viewing allowed hosts for a section.
func TestSecretsHostsView(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	// View hosts for a section
	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "api.example.com") || !strings.Contains(result.Text, "api.backup.com") {
		t.Errorf("expected hosts in output: %s", result.Text)
	}

	// View hosts for section without hosts
	result, _ = cmd.Execute(context.Background(), Request{Args: "hosts legacy"}, cc)
	if !strings.Contains(result.Text, "(none)") {
		t.Errorf("expected (none) for section without hosts: %s", result.Text)
	}
}

// TestSecretsHostsAdd verifies adding an allowed host to a section.
func TestSecretsHostsAdd(t *testing.T) {
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	cmd := SecretsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi add api.new.com"}, secretsCC(store))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Added") {
		t.Errorf("expected Added message: %s", result.Text)
	}
	if !store.saved {
		t.Error("expected Save() to be called")
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.new.com" {
		t.Errorf("hosts = %v", hosts)
	}
}

// TestSecretsHostsRemove verifies removing an allowed host from a section.
func TestSecretsHostsRemove(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi remove api.example.com"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Removed") {
		t.Errorf("expected Removed message: %s", result.Text)
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.backup.com" {
		t.Errorf("hosts after remove = %v", hosts)
	}

	// Remove nonexistent
	result, _ = cmd.Execute(context.Background(), Request{Args: "hosts myapi remove nonexistent.com"}, cc)
	if !strings.Contains(result.Text, "not found") {
		t.Errorf("expected not found message: %s", result.Text)
	}
}

// TestSecretsHostsClear verifies clearing all allowed hosts for a section.
func TestSecretsHostsClear(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com"},
		},
	}
	cmd := SecretsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi clear"}, secretsCC(store))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Cleared") {
		t.Errorf("expected Cleared message: %s", result.Text)
	}
	if store.SectionAllowedHosts("myapi") != nil {
		t.Error("hosts should be nil after clear")
	}
}

// TestSecretsHostsUsage verifies usage message for hosts subcommand.
func TestSecretsHostsUsage(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	// No args
	result, _ := cmd.Execute(context.Background(), Request{Args: "hosts"}, cc)
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage: %s", result.Text)
	}

	// Invalid action
	result, _ = cmd.Execute(context.Background(), Request{Args: "hosts myapi invalid"}, cc)
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage for invalid action: %s", result.Text)
	}
}
