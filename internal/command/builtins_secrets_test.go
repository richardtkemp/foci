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

// secretsCCWithDeps returns a CommandContext with SecretsDeps wired up.
func secretsCCWithDeps(store SecretsStore, reg *Registry) CommandContext {
	return CommandContext{
		SecretsStore: store,
		SecretsDeps: &SecretsDeps{
			Registry: reg,
			Store:    store,
		},
	}
}

// TestSecretsListTable verifies secrets are displayed in table format with section grouping.
func TestSecretsListTable(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"anthropic.setup_token": "x",
			"telegram.clutch":       "x",
			"telegram.clutchling":   "x",
			"telegram.scout":        "x",
			"brave.api_key":         "x",
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

// TestSecretsChainKeyboardSet verifies chain keyboard for the set subcommand.
func TestSecretsChainKeyboardSet(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"anthropic.api_key": "x",
			"anthropic.token":   "x",
			"custom.secret":     "x",
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)
	ctx := context.Background()

	// "set" → section buttons
	opts := cmd.ChainKeyboard(ctx, "set", cc)
	if len(opts) != 2 {
		t.Fatalf("set: got %d options, want 2", len(opts))
	}
	// Sorted: anthropic, custom
	if opts[0].Label != "anthropic" || opts[0].Data != "set anthropic" {
		t.Errorf("set opts[0] = %+v", opts[0])
	}
	if opts[1].Label != "custom" || opts[1].Data != "set custom" {
		t.Errorf("set opts[1] = %+v", opts[1])
	}

	// "set anthropic" → key buttons
	opts = cmd.ChainKeyboard(ctx, "set anthropic", cc)
	if len(opts) != 2 {
		t.Fatalf("set anthropic: got %d options, want 2", len(opts))
	}
	// Sorted: api_key, token
	if opts[0].Label != "api_key" || opts[0].Data != "set anthropic api_key" {
		t.Errorf("set anthropic opts[0] = %+v", opts[0])
	}

	// "set anthropic api_key" → no further chaining (wizard takes over)
	opts = cmd.ChainKeyboard(ctx, "set anthropic api_key", cc)
	if opts != nil {
		t.Errorf("set anthropic api_key should not chain, got %v", opts)
	}
}

// TestSecretsChainKeyboardRemove verifies chain keyboard for the remove subcommand.
func TestSecretsChainKeyboardRemove(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"myapi.key1": "x",
			"myapi.key2": "x",
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)
	ctx := context.Background()

	// "remove" → section buttons
	opts := cmd.ChainKeyboard(ctx, "remove", cc)
	if len(opts) != 1 || opts[0].Label != "myapi" {
		t.Errorf("remove opts = %+v", opts)
	}

	// "remove myapi" → key buttons
	opts = cmd.ChainKeyboard(ctx, "remove myapi", cc)
	if len(opts) != 2 {
		t.Fatalf("remove myapi: got %d options, want 2", len(opts))
	}
	if opts[0].Data != "remove myapi key1" {
		t.Errorf("remove myapi opts[0] = %+v", opts[0])
	}

	// "remove myapi key1" → no further chaining (executes remove)
	opts = cmd.ChainKeyboard(ctx, "remove myapi key1", cc)
	if opts != nil {
		t.Errorf("remove myapi key1 should not chain, got %v", opts)
	}
}

// TestSecretsChainKeyboardHosts verifies chain keyboard for the hosts subcommand.
func TestSecretsChainKeyboardHosts(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"myapi.token": "x",
		},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)
	ctx := context.Background()

	// "hosts" → section buttons
	opts := cmd.ChainKeyboard(ctx, "hosts", cc)
	if len(opts) != 1 || opts[0].Label != "myapi" {
		t.Errorf("hosts opts = %+v", opts)
	}

	// "hosts myapi" → action buttons [add, remove, clear]
	opts = cmd.ChainKeyboard(ctx, "hosts myapi", cc)
	if len(opts) != 3 {
		t.Fatalf("hosts myapi: got %d options, want 3", len(opts))
	}
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.Label
	}
	if strings.Join(labels, ",") != "add,remove,clear" {
		t.Errorf("hosts myapi labels = %v", labels)
	}

	// "hosts myapi remove" → host buttons
	opts = cmd.ChainKeyboard(ctx, "hosts myapi remove", cc)
	if len(opts) != 2 {
		t.Fatalf("hosts myapi remove: got %d options, want 2", len(opts))
	}
	if opts[0].Label != "api.example.com" || opts[0].Data != "hosts myapi remove api.example.com" {
		t.Errorf("hosts myapi remove opts[0] = %+v", opts[0])
	}

	// "hosts myapi add" → no further chaining (wizard takes over)
	opts = cmd.ChainKeyboard(ctx, "hosts myapi add", cc)
	if opts != nil {
		t.Errorf("hosts myapi add should not chain, got %v", opts)
	}

	// "hosts myapi clear" → no further chaining (executes clear)
	opts = cmd.ChainKeyboard(ctx, "hosts myapi clear", cc)
	if opts != nil {
		t.Errorf("hosts myapi clear should not chain, got %v", opts)
	}
}

// TestSecretsChainKeyboardNoStore verifies ChainKeyboard returns nil when no store.
func TestSecretsChainKeyboardNoStore(t *testing.T) {
	cmd := SecretsCommand()
	cc := CommandContext{} // no store
	opts := cmd.ChainKeyboard(context.Background(), "set", cc)
	if opts != nil {
		t.Errorf("expected nil with no store, got %v", opts)
	}
}

// TestSecretsRemoveViaKeyboard verifies "remove section key" from keyboard chain works.
func TestSecretsRemoveViaKeyboard(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"myapi.token": "x",
			"myapi.key":   "x",
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "remove myapi token"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "removed") {
		t.Errorf("expected removed message: %s", result.Text)
	}
	if _, ok := store.data["myapi.token"]; ok {
		t.Error("myapi.token should have been removed")
	}
	// Other key should remain.
	if _, ok := store.data["myapi.key"]; !ok {
		t.Error("myapi.key should still exist")
	}
}

// TestSecretsSetWizardBare verifies the full set wizard flow from bare /secrets set.
func TestSecretsSetWizardBare(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	// Start wizard with bare "set"
	result, err := cmd.Execute(context.Background(), Request{Args: "set"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "section.key") {
		t.Errorf("expected section.key prompt: %s", result.Text)
	}

	// Step 0: enter section.key
	resp, _, handled := reg.HandleMessage("custom.api_key")
	if !handled {
		t.Fatal("wizard should handle the message")
	}
	if !strings.Contains(resp, "Enter value") {
		t.Errorf("expected value prompt: %s", resp)
	}

	// Step 1: enter value
	resp, _, handled = reg.HandleMessage("my-secret-value")
	if !handled {
		t.Fatal("wizard should handle the message")
	}
	if !strings.Contains(resp, "Secret custom.api_key set") {
		t.Errorf("expected set confirmation: %s", resp)
	}
	if !strings.Contains(resp, "allowed_hosts") {
		t.Errorf("expected hosts prompt: %s", resp)
	}

	// Verify the secret was set.
	if store.data["custom.api_key"] != "my-secret-value" {
		t.Errorf("secret not set: %v", store.data)
	}

	// Step 2: set hosts
	resp, _, handled = reg.HandleMessage("api.example.com, api.backup.com")
	if !handled {
		t.Fatal("wizard should handle the message")
	}
	if !strings.Contains(resp, "api.example.com") {
		t.Errorf("expected hosts confirmation: %s", resp)
	}

	// Wizard should be done.
	_, _, handled = reg.HandleMessage("anything")
	if handled {
		t.Error("wizard should be cleared after completion")
	}

	// Verify hosts were set.
	hosts := store.SectionAllowedHosts("custom")
	if len(hosts) != 2 {
		t.Errorf("expected 2 hosts, got %v", hosts)
	}
}

// TestSecretsSetWizardKeyboardFastForward verifies wizard fast-forwards when section+key
// are pre-populated from keyboard chain.
func TestSecretsSetWizardKeyboardFastForward(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{
		"myapi.token": "old-value",
	}}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	// "set myapi token" — from keyboard chain.
	result, err := cmd.Execute(context.Background(), Request{Args: "set myapi token"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Enter value") {
		t.Errorf("expected value prompt: %s", result.Text)
	}
	if !strings.Contains(result.Text, "myapi.token") {
		t.Errorf("expected section.key in prompt: %s", result.Text)
	}

	// Enter value.
	resp, _, handled := reg.HandleMessage("new-secret")
	if !handled {
		t.Fatal("wizard should handle the message")
	}
	if !strings.Contains(resp, "Secret myapi.token set") {
		t.Errorf("expected set confirmation: %s", resp)
	}

	// Verify secret was updated.
	if store.data["myapi.token"] != "new-secret" {
		t.Errorf("secret not updated: %v", store.data)
	}

	// Skip hosts with /stop.
	stopResp, _, handled := reg.HandleMessage("/stop")
	if !handled {
		t.Fatal("/stop should be handled")
	}
	if !strings.Contains(stopResp, "cancelled") {
		t.Errorf("expected cancelled: %s", stopResp)
	}
}

// TestSecretsSetWizardSkipHosts verifies wizard skips hosts when /stop is entered.
func TestSecretsSetWizardSkipHosts(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	// Start wizard
	cmd.Execute(context.Background(), Request{Args: "set"}, cc)

	// Enter section.key
	reg.HandleMessage("custom.key")

	// Enter value
	resp, _, handled := reg.HandleMessage("my-value")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "allowed_hosts") {
		t.Errorf("expected hosts prompt: %s", resp)
	}

	// /stop to skip hosts
	_, _, handled = reg.HandleMessage("/stop")
	if !handled {
		t.Fatal("/stop should be handled")
	}

	// Secret should still be set.
	if store.data["custom.key"] != "my-value" {
		t.Errorf("secret should be set: %v", store.data)
	}
}

// TestSecretsSetWizardInvalidName verifies wizard rejects invalid names.
func TestSecretsSetWizardInvalidName(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	cmd.Execute(context.Background(), Request{Args: "set"}, cc)

	// Empty name
	resp, _, handled := reg.HandleMessage("")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "cannot be empty") {
		t.Errorf("expected empty error: %s", resp)
	}

	// Name without dot
	resp, _, handled = reg.HandleMessage("nodot")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "section.key") {
		t.Errorf("expected format error: %s", resp)
	}

	// Valid name — wizard should advance
	resp, _, handled = reg.HandleMessage("test.key")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "Enter value") {
		t.Errorf("expected value prompt: %s", resp)
	}
}

// TestSecretsHostsAddWizard verifies the hosts-add wizard flow.
func TestSecretsHostsAddWizard(t *testing.T) {
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	// "hosts myapi add" with no host — activates wizard.
	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi add"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Enter host") {
		t.Errorf("expected host prompt: %s", result.Text)
	}

	// Enter host.
	resp, _, handled := reg.HandleMessage("api.new.com")
	if !handled {
		t.Fatal("wizard should handle the message")
	}
	if !strings.Contains(resp, "Added api.new.com") {
		t.Errorf("expected added confirmation: %s", resp)
	}

	// Verify host was added.
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.new.com" {
		t.Errorf("hosts = %v", hosts)
	}

	// Wizard should be done.
	_, _, handled = reg.HandleMessage("anything")
	if handled {
		t.Error("wizard should be cleared after completion")
	}
}

// TestSecretsHostsAddWizardCancel verifies wizard cancellation.
func TestSecretsHostsAddWizardCancel(t *testing.T) {
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	cmd.Execute(context.Background(), Request{Args: "hosts myapi add"}, cc)

	// Cancel with /cancel
	resp, _, handled := reg.HandleMessage("/cancel")
	if !handled {
		t.Fatal("/cancel should be handled")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("expected cancelled: %s", resp)
	}

	// No hosts should be added.
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 0 {
		t.Errorf("no hosts should be added after cancel: %v", hosts)
	}
}

// TestSecretsHostsRemoveViaKeyboard verifies removing a host via keyboard chain.
func TestSecretsHostsRemoveViaKeyboard(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	// "hosts myapi remove api.example.com" — direct from keyboard chain.
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
}

// TestSecretsHostsClearViaKeyboard verifies clearing hosts via keyboard chain.
func TestSecretsHostsClearViaKeyboard(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi clear"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Cleared") {
		t.Errorf("expected Cleared message: %s", result.Text)
	}
}

// TestSecretsSetDirectWithValue verifies direct "set section.key value" still works.
func TestSecretsSetDirectWithValue(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "set custom.api_key my-secret-value"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "set") {
		t.Errorf("expected set message: %s", result.Text)
	}
	if store.data["custom.api_key"] != "my-secret-value" {
		t.Errorf("secret not set: %v", store.data)
	}
}

// TestSecretsSetDirectMultiWordValue verifies "set section key value with spaces" from keyboard.
func TestSecretsSetDirectMultiWordValue(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	// "set custom api_key my secret value" — 3+ args, first without dot.
	result, err := cmd.Execute(context.Background(), Request{Args: "set custom api_key my secret value"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "set") {
		t.Errorf("expected set message: %s", result.Text)
	}
	if store.data["custom.api_key"] != "my secret value" {
		t.Errorf("secret not set: %v", store.data)
	}
}

// TestSecretSections verifies the helper returns sorted unique sections.
func TestSecretSections(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"b.key1": "x",
			"a.key1": "x",
			"a.key2": "x",
			"c.key1": "x",
		},
	}
	sections := secretSections(store)
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(sections))
	}
	if sections[0] != "a" || sections[1] != "b" || sections[2] != "c" {
		t.Errorf("sections = %v, want [a b c]", sections)
	}
}

// TestSecretKeysInSection verifies the helper returns sorted keys within a section.
func TestSecretKeysInSection(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"myapi.token":   "x",
			"myapi.api_key": "x",
			"other.key":     "x",
		},
	}
	keys := secretKeysInSection(store, "myapi")
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}
	if keys[0] != "api_key" || keys[1] != "token" {
		t.Errorf("keys = %v, want [api_key token]", keys)
	}

	// Non-existent section
	keys = secretKeysInSection(store, "nonexistent")
	if len(keys) != 0 {
		t.Errorf("expected no keys for nonexistent section, got %v", keys)
	}
}

// TestSecretsHostsAddWizardEmptyHost verifies wizard rejects empty host input.
func TestSecretsHostsAddWizardEmptyHost(t *testing.T) {
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	cmd.Execute(context.Background(), Request{Args: "hosts myapi add"}, cc)

	// Empty host
	resp, _, handled := reg.HandleMessage("")
	if !handled {
		t.Fatal("wizard should handle empty input")
	}
	if !strings.Contains(resp, "cannot be empty") {
		t.Errorf("expected empty error: %s", resp)
	}

	// Now enter a valid host
	resp, _, handled = reg.HandleMessage("api.valid.com")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "Added") {
		t.Errorf("expected added: %s", resp)
	}
}

// TestSecretsSetWizardEmptyValue verifies wizard rejects empty value.
func TestSecretsSetWizardEmptyValue(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	reg := NewRegistry()
	cc := secretsCCWithDeps(store, reg)
	cmd := SecretsCommand()

	cmd.Execute(context.Background(), Request{Args: "set"}, cc)
	reg.HandleMessage("test.key") // step 0 → step 1

	// Empty value
	resp, _, handled := reg.HandleMessage("")
	if !handled {
		t.Fatal("wizard should handle")
	}
	if !strings.Contains(resp, "cannot be empty") {
		t.Errorf("expected empty error: %s", resp)
	}
}

// TestSecretsSetNoRegistry verifies graceful fallback when no registry.
func TestSecretsSetNoRegistry(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cc := secretsCC(store) // no SecretsDeps → no registry
	cmd := SecretsCommand()

	// Bare "set" without registry should show usage.
	result, err := cmd.Execute(context.Background(), Request{Args: "set"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage: %s", result.Text)
	}
}

// TestSecretsHostsAddNoRegistry verifies graceful fallback when no registry.
func TestSecretsHostsAddNoRegistry(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cc := secretsCC(store) // no SecretsDeps → no registry
	cmd := SecretsCommand()

	// "hosts myapi add" without registry should show usage.
	result, err := cmd.Execute(context.Background(), Request{Args: "hosts myapi add"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage fallback: %s", result.Text)
	}
}
