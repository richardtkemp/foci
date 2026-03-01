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
