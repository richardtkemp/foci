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
		cmd := NewConfigCommand(func(args string) (string, error) { return args, nil })
		if cmd.KeyboardOptions == nil {
			t.Fatal("config command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background())
		if len(opts) != 3 {
			t.Fatalf("got %d options, want 3", len(opts))
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
			func(context.Context, string) {},
			func(s string) string { return s },
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
			func(context.Context, string) {},
			func(s string) string { return s },
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

	if cmd.SkipToolExport != true {
		t.Error("secrets command must have SkipToolExport=true")
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
