package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRegistryDispatch(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:        "test",
		Description: "test command",
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return "no args", nil
			}
			return "args: " + args, nil
		},
	})

	ctx := context.Background()

	// Basic dispatch
	result, ok := r.Dispatch(ctx, "/test")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "no args" {
		t.Errorf("result = %q", result)
	}

	// With args
	result, ok = r.Dispatch(ctx, "/test hello world")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "args: hello world" {
		t.Errorf("result = %q", result)
	}

	// Unknown command — now returns suggestion
	result, ok = r.Dispatch(ctx, "/unknown")
	if !ok {
		t.Error("expected unknown command to be handled (with suggestion)")
	}
	if !strings.Contains(result, "Unknown command") {
		t.Errorf("expected suggestion, got %q", result)
	}

	// Not a command
	_, ok = r.Dispatch(ctx, "regular message")
	if ok {
		t.Error("expected non-command to return false")
	}
}

func TestDispatchCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "ping",
		Execute: func(ctx context.Context, args string) (string, error) { return "pong", nil },
	})

	result, ok := r.Dispatch(context.Background(), "/PING")
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
	if result != "pong" {
		t.Errorf("result = %q", result)
	}
}

func TestDispatchError(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "fail",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "", fmt.Errorf("something broke")
		},
	})

	result, ok := r.Dispatch(context.Background(), "/fail")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "Error: something broke" {
		t.Errorf("result = %q", result)
	}
}

func TestDispatchUnknownSuggestion(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "status",
		Execute: func(ctx context.Context, args string) (string, error) { return "", nil },
	})
	r.Register(&Command{
		Name:    "sessions",
		Execute: func(ctx context.Context, args string) (string, error) { return "", nil },
	})
	r.Register(&Command{
		Name:    "ping",
		Execute: func(ctx context.Context, args string) (string, error) { return "", nil },
	})

	tests := []struct {
		name    string
		input   string
		wantIn  string // expected substring in result
	}{
		{"close typo", "/statsu", "/status"},
		{"close typo 2", "/staus", "/status"},
		{"prefix match", "/ses", "/sessions"},
		{"no match", "/xyzzy", "/help"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := r.Dispatch(context.Background(), tt.input)
			if !ok {
				t.Fatal("expected unknown command to be handled")
			}
			if !strings.Contains(result, tt.wantIn) {
				t.Errorf("result = %q, want containing %q", result, tt.wantIn)
			}
		})
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"status", "statsu", 2},
		{"ping", "pong", 1},
		{"cat", "dog", 3},
	}
	for _, tt := range tests {
		got := levenshtein(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLookupKeyboard(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "model",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "executed: " + args, nil
		},
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{
				{Label: "haiku", Data: "haiku"},
				{Label: "sonnet", Data: "sonnet"},
				{Label: "opus", Data: "opus"},
			}
		},
	})
	r.Register(&Command{
		Name: "ping",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "pong", nil
		},
		// No KeyboardOptions — should never return keyboard
	})

	ctx := context.Background()

	// Bare command with keyboard options → returns keyboard
	name, opts, ok := r.LookupKeyboard(ctx, "/model")
	if !ok {
		t.Fatal("expected keyboard for bare /model")
	}
	if name != "model" {
		t.Errorf("name = %q, want model", name)
	}
	if len(opts) != 3 {
		t.Fatalf("got %d options, want 3", len(opts))
	}
	if opts[0].Label != "haiku" || opts[0].Data != "haiku" {
		t.Errorf("opts[0] = %+v", opts[0])
	}

	// Command with args → no keyboard (execute normally)
	_, _, ok = r.LookupKeyboard(ctx, "/model sonnet")
	if ok {
		t.Error("should not return keyboard when args provided")
	}

	// Command without keyboard options → no keyboard
	_, _, ok = r.LookupKeyboard(ctx, "/ping")
	if ok {
		t.Error("should not return keyboard for command without KeyboardOptions")
	}

	// Unknown command → no keyboard
	_, _, ok = r.LookupKeyboard(ctx, "/unknown")
	if ok {
		t.Error("should not return keyboard for unknown command")
	}

	// Not a command → no keyboard
	_, _, ok = r.LookupKeyboard(ctx, "regular message")
	if ok {
		t.Error("should not return keyboard for non-command")
	}

	// Dispatch still works with args (keyboard doesn't block normal dispatch)
	result, dispatched := r.Dispatch(ctx, "/model opus")
	if !dispatched {
		t.Fatal("expected dispatch to succeed")
	}
	if result != "executed: opus" {
		t.Errorf("result = %q", result)
	}
}

func TestLookupKeyboardCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "effort",
		Execute: func(ctx context.Context, args string) (string, error) { return "", nil },
		KeyboardOptions: func(ctx context.Context) []KeyboardOption {
			return []KeyboardOption{{Label: "low", Data: "low"}}
		},
	})

	_, _, ok := r.LookupKeyboard(context.Background(), "/EFFORT")
	if !ok {
		t.Error("keyboard lookup should be case-insensitive")
	}
}

func TestKeyboardOptionsOnBuiltinCommands(t *testing.T) {
	// Verify the builtin commands that should have keyboards do have them
	t.Run("effort", func(t *testing.T) {
		cmd := NewEffortCommand(
			func(context.Context) string { return "medium" },
			func(context.Context, string) {},
		)
		if cmd.KeyboardOptions == nil {
			t.Fatal("effort command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 3 {
			t.Fatalf("got %d options, want 3", len(opts))
		}
		// Check current value is marked
		found := false
		for _, o := range opts {
			if strings.Contains(o.Label, "✓") {
				found = true
				if !strings.HasPrefix(o.Label, "medium") {
					t.Errorf("wrong option marked: %q", o.Label)
				}
			}
		}
		if !found {
			t.Error("current effort should be marked with ✓")
		}
	})

	t.Run("thinking", func(t *testing.T) {
		cmd := NewThinkingCommand(
			func(context.Context) string { return "adaptive" },
			func(context.Context, string) {},
		)
		if cmd.KeyboardOptions == nil {
			t.Fatal("thinking command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})

	t.Run("config", func(t *testing.T) {
		cmd := NewConfigCommand(func(ctx context.Context, args string) (string, error) { return args, nil }, nil, nil)
		if cmd.KeyboardOptions == nil {
			t.Fatal("config command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 4 {
			t.Fatalf("got %d options, want 4", len(opts))
		}
	})

	t.Run("model_with_aliases", func(t *testing.T) {
		aliases := map[string]string{
			"haiku":  "claude-haiku-4-5",
			"sonnet": "claude-sonnet-4-6",
			"opus":   "claude-opus-4-6",
		}
		cmd := NewModelCommand(
			func(context.Context) string { return "claude-sonnet-4-6" },
			func(context.Context, string, string, string) {},
			func(s string) (string, string, string) { return "", s, "anthropic" },
			aliases,
		)
		if cmd.KeyboardOptions == nil {
			t.Fatal("model command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 3 {
			t.Fatalf("got %d options, want 3", len(opts))
		}
		// sonnet should be marked as current
		found := false
		for _, o := range opts {
			if strings.Contains(o.Label, "✓") {
				found = true
				if !strings.HasPrefix(o.Label, "sonnet") {
					t.Errorf("wrong option marked: %q", o.Label)
				}
			}
		}
		if !found {
			t.Error("current model alias should be marked with ✓")
		}
	})

	t.Run("model_no_aliases", func(t *testing.T) {
		cmd := NewModelCommand(
			func(context.Context) string { return "claude-opus-4-6" },
			func(context.Context, string, string, string) {},
			func(s string) (string, string, string) { return "", s, "anthropic" },
			nil,
		)
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 3 {
			t.Fatalf("got %d options, want 3", len(opts))
		}
		// opus should be marked
		found := false
		for _, o := range opts {
			if strings.Contains(o.Label, "✓") && strings.HasPrefix(o.Label, "opus") {
				found = true
			}
		}
		if !found {
			t.Error("current model should be marked with ✓")
		}
	})

	t.Run("cost", func(t *testing.T) {
		// Verify /cost has keyboard options for its subcommands.
		cmd := NewCostCommand(t.TempDir() + "/empty.jsonl")
		if cmd.KeyboardOptions == nil {
			t.Fatal("cost command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		wantData := map[string]bool{"today": false, "24h": false, "week": false}
		for _, o := range opts {
			if _, ok := wantData[o.Data]; ok {
				wantData[o.Data] = true
			}
		}
		for k, found := range wantData {
			if !found {
				t.Errorf("missing keyboard option for %q", k)
			}
		}
	})

	t.Run("compact", func(t *testing.T) {
		// Verify /compact has keyboard options for run and dry-run.
		cmd := NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) { return 0, nil })
		if cmd.KeyboardOptions == nil {
			t.Fatal("compact command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})

	t.Run("bitwarden", func(t *testing.T) {
		// Verify /bitwarden has keyboard options for status and setup.
		cmd := NewBitwardenCommand(nil, false)
		if cmd.KeyboardOptions == nil {
			t.Fatal("bitwarden command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})
}

func TestLookupChainKeyboard(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "tmux",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "executed: " + args, nil
		},
		ChainKeyboard: func(ctx context.Context, subcommand string) []KeyboardOption {
			if subcommand == "kill" {
				return []KeyboardOption{
					{Label: "sess-1", Data: "kill sess-1"},
					{Label: "sess-2", Data: "kill sess-2"},
				}
			}
			return nil
		},
	})
	r.Register(&Command{
		Name:    "ping",
		Execute: func(ctx context.Context, args string) (string, error) { return "pong", nil },
		// No ChainKeyboard
	})

	ctx := context.Background()

	// Bare subcommand with chain → returns options
	name, opts, ok := r.LookupChainKeyboard(ctx, "/tmux kill")
	if !ok {
		t.Fatal("expected chain keyboard for /tmux kill")
	}
	if name != "tmux" {
		t.Errorf("name = %q, want tmux", name)
	}
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2", len(opts))
	}
	if opts[0].Label != "sess-1" {
		t.Errorf("opts[0].Label = %q", opts[0].Label)
	}

	// Subcommand with no chain options → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux list")
	if ok {
		t.Error("should not chain for /tmux list (ChainKeyboard returns nil)")
	}

	// Full args (already has parameter) → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux kill mysession")
	if ok {
		t.Error("should not chain when full args provided")
	}

	// Bare command (no subcommand) → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux")
	if ok {
		t.Error("should not chain for bare command with no subcommand")
	}

	// Command without ChainKeyboard → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/ping something")
	if ok {
		t.Error("should not chain for command without ChainKeyboard")
	}

	// Not a command → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "regular message")
	if ok {
		t.Error("should not chain for non-command")
	}
}

func TestAll(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{Name: "beta"})
	r.Register(&Command{Name: "alpha"})

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("got %d commands", len(all))
	}
	if all[0].Name != "alpha" {
		t.Errorf("first = %s, want alpha (sorted)", all[0].Name)
	}
}

// mockSecretsStore implements SecretsStore for testing.
type mockSecretsStore struct {
	data         map[string]string
	allowedHosts map[string][]string
	saved        bool
}

func (m *mockSecretsStore) Names() []string {
	names := make([]string, 0, len(m.data))
	for k := range m.data {
		names = append(names, k)
	}
	return names
}
func (m *mockSecretsStore) Set(name, value string) { m.data[name] = value }
func (m *mockSecretsStore) Remove(name string) bool {
	if _, ok := m.data[name]; !ok {
		return false
	}
	delete(m.data, name)
	return true
}
func (m *mockSecretsStore) Save() error { m.saved = true; return nil }
func (m *mockSecretsStore) SectionAllowedHosts(section string) []string {
	if m.allowedHosts == nil {
		return nil
	}
	return m.allowedHosts[section]
}
func (m *mockSecretsStore) AddAllowedHost(section, host string) {
	if m.allowedHosts == nil {
		m.allowedHosts = make(map[string][]string)
	}
	host = strings.ToLower(strings.TrimSpace(host))
	m.allowedHosts[section] = append(m.allowedHosts[section], host)
}
func (m *mockSecretsStore) RemoveAllowedHost(section, host string) bool {
	hosts := m.allowedHosts[section]
	for i, h := range hosts {
		if strings.EqualFold(h, host) {
			m.allowedHosts[section] = append(hosts[:i], hosts[i+1:]...)
			return true
		}
	}
	return false
}
func (m *mockSecretsStore) SetAllowedHosts(section string, hosts []string) {
	if m.allowedHosts == nil {
		m.allowedHosts = make(map[string][]string)
	}
	if len(hosts) == 0 {
		delete(m.allowedHosts, section)
	} else {
		m.allowedHosts[section] = hosts
	}
}

func TestRestartCommand(t *testing.T) {
	var notified string
	cmd := NewRestartCommand(func(msg string) {
		notified = msg
	})

	if cmd.Name != "restart" {
		t.Errorf("name = %q, want restart", cmd.Name)
	}

	// We can't actually restart in tests, but verify the notify callback fires.
	// The command calls exec.Command("systemctl", ...) which may fail in test env.
	// Just verify the command exists and has the right properties.
	if cmd.Description == "" {
		t.Error("description should not be empty")
	}

	// Test with nil notifyFn (should not panic)
	cmdNoNotify := NewRestartCommand(nil)
	if cmdNoNotify.Name != "restart" {
		t.Errorf("name = %q", cmdNoNotify.Name)
	}

	// Verify notifyFn is called if set
	_ = notified // will be tested when we can mock systemctl
}

func TestSecretsCommand(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{
		"anthropic.setup_token": "sk-ant-123",
		"custom.api_key":  "key-456",
	}}
	cmd := NewSecretsCommand(store)

	if cmd.KeyboardOptions == nil {
		t.Fatal("secrets command should have KeyboardOptions")
	}
	opts := cmd.KeyboardOptions(context.Background())
	wantLabels := []string{"list", "set", "remove"}
	if len(opts) != len(wantLabels) {
		t.Fatalf("got %d keyboard options, want %d", len(opts), len(wantLabels))
	}
	for i, want := range wantLabels {
		if opts[i].Label != want || opts[i].Data != want {
			t.Errorf("option %d = {%q, %q}, want {%q, %q}", i, opts[i].Label, opts[i].Data, want, want)
		}
	}

	// List
	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result, "anthropic") || !strings.Contains(result, "token") {
		t.Errorf("list result = %q, want anthropic section with token", result)
	}
	// Secret values must never appear
	if strings.Contains(result, "sk-ant-123") || strings.Contains(result, "key-456") {
		t.Error("list should not display secret values")
	}

	// Set
	result, err = cmd.Execute(context.Background(), "set custom.new_key my-secret-value")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(result, "set") {
		t.Errorf("set result = %q", result)
	}
	if store.data["custom.new_key"] != "my-secret-value" {
		t.Errorf("key not set: %v", store.data)
	}
	if !store.saved {
		t.Error("Save should have been called")
	}

	// Remove
	store.saved = false
	result, err = cmd.Execute(context.Background(), "remove custom.api_key")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(result, "removed") {
		t.Errorf("remove result = %q", result)
	}
	if _, ok := store.data["custom.api_key"]; ok {
		t.Error("key should be removed")
	}
	if !store.saved {
		t.Error("Save should have been called")
	}

	// Remove nonexistent
	result, err = cmd.Execute(context.Background(), "remove nonexistent.key")
	if err != nil {
		t.Fatalf("remove nonexistent: %v", err)
	}
	if !strings.Contains(result, "not found") {
		t.Errorf("remove nonexistent result = %q", result)
	}

	// Usage (no args)
	result, err = cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("no args: %v", err)
	}
	if !strings.Contains(result, "Usage") {
		t.Errorf("empty args result = %q, want usage", result)
	}
}

type mockWizard struct {
	responses map[string]string
	done      bool
}

func (m *mockWizard) Handle(text string) (string, bool) {
	if response, ok := m.responses[text]; ok {
		return response, m.done
	}
	return "no response", m.done
}

func TestSetWizard(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{
		responses: map[string]string{
			"hello": "hi there",
		},
		done: false,
	}

	reg.SetWizard(wizard)

	// Verify wizard was set by calling HandleMessage
	resp, handled := reg.HandleMessage("hello")
	if !handled {
		t.Error("HandleMessage should indicate wizard handled the message")
	}
	if resp != "hi there" {
		t.Errorf("response = %q, want %q", resp, "hi there")
	}
}

func TestClearWizard(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{
		responses: map[string]string{
			"test": "response",
		},
	}

	reg.SetWizard(wizard)
	reg.ClearWizard()

	// After clearing, HandleMessage should not handle messages
	_, handled := reg.HandleMessage("test")
	if handled {
		t.Error("HandleMessage should not handle after wizard is cleared")
	}
}

func TestHandleMessageWizardCancel(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{
		responses: map[string]string{},
	}

	reg.SetWizard(wizard)
	resp, handled := reg.HandleMessage("/cancel")

	if !handled {
		t.Error("HandleMessage should handle /cancel")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("response = %q, want 'cancelled'", resp)
	}

	// Wizard should be cleared
	_, handled = reg.HandleMessage("test")
	if handled {
		t.Error("wizard should be cleared after /cancel")
	}
}

func TestHandleMessageWizardStop(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{
		responses: map[string]string{},
	}

	reg.SetWizard(wizard)
	resp, handled := reg.HandleMessage("/stop")

	if !handled {
		t.Error("HandleMessage should handle /stop")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("response = %q, want 'cancelled'", resp)
	}

	// Wizard should be cleared
	_, handled = reg.HandleMessage("test")
	if handled {
		t.Error("wizard should be cleared after /stop")
	}
}

func TestHandleMessageWizardDone(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{
		responses: map[string]string{
			"input": "output",
		},
		done: true, // Wizard finishes after this handle
	}

	reg.SetWizard(wizard)
	resp, handled := reg.HandleMessage("input")

	if !handled {
		t.Error("HandleMessage should handle the message")
	}
	if resp != "output" {
		t.Errorf("response = %q, want %q", resp, "output")
	}

	// Wizard should be cleared since it returned done=true
	_, handled = reg.HandleMessage("another")
	if handled {
		t.Error("wizard should be cleared when it returns done=true")
	}
}
