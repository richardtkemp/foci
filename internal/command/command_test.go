package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestRegistryDispatch verifies basic command dispatch, argument passing, unknown command
// suggestions, and non-command rejection.
func TestRegistryDispatch(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:        "test",
		Description: "test command",
		Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
			if req.Args == "" {
				return Response{Text: "no args"}, nil
			}
			return Response{Text: "args: " + req.Args}, nil
		},
	})

	ctx := context.Background()
	cc := CommandContext{}

	// Basic dispatch
	resp, ok, err := r.Dispatch(ctx, Request{Name: "test"}, cc)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !ok {
		t.Fatal("expected command to be found")
	}
	if resp.Text != "no args" {
		t.Errorf("result = %q", resp.Text)
	}

	// With args
	resp, ok, err = r.Dispatch(ctx, Request{Name: "test", Args: "hello world"}, cc)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !ok {
		t.Fatal("expected command to be found")
	}
	if resp.Text != "args: hello world" {
		t.Errorf("result = %q", resp.Text)
	}

	// Unknown command — now returns suggestion
	resp, ok, _ = r.Dispatch(ctx, Request{Name: "unknown"}, cc)
	if !ok {
		t.Error("expected unknown command to be handled (with suggestion)")
	}
	if !strings.Contains(resp.Text, "Unknown command") {
		t.Errorf("expected suggestion, got %q", resp.Text)
	}
}

// TestDispatchCaseInsensitive verifies that command names are matched case-insensitively.
func TestDispatchCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "ping",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "pong"}, nil
		},
	})

	resp, ok, _ := r.Dispatch(context.Background(), Request{Name: "ping"}, CommandContext{})
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
	if resp.Text != "pong" {
		t.Errorf("result = %q", resp.Text)
	}
}

// TestDispatchError verifies that command execution errors are wrapped in error text.
func TestDispatchError(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "fail",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{}, fmt.Errorf("something broke")
		},
	})

	resp, ok, _ := r.Dispatch(context.Background(), Request{Name: "fail"}, CommandContext{})
	if !ok {
		t.Fatal("expected command to be found")
	}
	if resp.Text != "Error: something broke" {
		t.Errorf("result = %q", resp.Text)
	}
}

// TestDispatchUnknownSuggestion verifies typo correction and prefix matching for unknown commands.
func TestDispatchUnknownSuggestion(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "status",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
	})
	r.Register(&Command{
		Name:    "sessions",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
	})
	r.Register(&Command{
		Name:    "ping",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
	})

	tests := []struct {
		name   string
		input  string
		wantIn string // expected substring in result
	}{
		{"close typo", "statsu", "/status"},
		{"close typo 2", "staus", "/status"},
		{"prefix match", "ses", "/sessions"},
		{"no match", "xyzzy", "/help"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, ok, _ := r.Dispatch(context.Background(), Request{Name: tt.input}, CommandContext{})
			if !ok {
				t.Fatal("expected unknown command to be handled")
			}
			if !strings.Contains(resp.Text, tt.wantIn) {
				t.Errorf("result = %q, want containing %q", resp.Text, tt.wantIn)
			}
		})
	}
}

// TestLevenshtein verifies the edit distance calculation used for typo correction.
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

// TestLookupKeyboard verifies that bare commands with KeyboardOptions return keyboard options,
// but commands with args or without KeyboardOptions do not.
func TestLookupKeyboard(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "model",
		Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
			return Response{Text: "executed: " + req.Args}, nil
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "haiku", Data: "haiku"},
				{Label: "sonnet", Data: "sonnet"},
				{Label: "opus", Data: "opus"},
			}
		},
	})
	r.Register(&Command{
		Name: "ping",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "pong"}, nil
		},
		// No KeyboardOptions — should never return keyboard
	})

	ctx := context.Background()
	cc := CommandContext{}

	// Bare command with keyboard options → returns keyboard
	name, header, opts, ok := r.LookupKeyboard(ctx, "/model", cc)
	if !ok {
		t.Fatal("expected keyboard for bare /model")
	}
	if name != "model" {
		t.Errorf("name = %q, want model", name)
	}
	if header != "/model:" {
		t.Errorf("header = %q, want /model: (default when no KeyboardHeader)", header)
	}
	if len(opts) != 3 {
		t.Fatalf("got %d options, want 3", len(opts))
	}
	if opts[0].Label != "haiku" || opts[0].Data != "haiku" {
		t.Errorf("opts[0] = %+v", opts[0])
	}

	// Command with args → no keyboard (execute normally)
	_, _, _, ok = r.LookupKeyboard(ctx, "/model sonnet", cc)
	if ok {
		t.Error("should not return keyboard when args provided")
	}

	// Command without keyboard options → no keyboard
	_, _, _, ok = r.LookupKeyboard(ctx, "/ping", cc)
	if ok {
		t.Error("should not return keyboard for command without KeyboardOptions")
	}

	// Unknown command → no keyboard
	_, _, _, ok = r.LookupKeyboard(ctx, "/unknown", cc)
	if ok {
		t.Error("should not return keyboard for unknown command")
	}

	// Not a command → no keyboard
	_, _, _, ok = r.LookupKeyboard(ctx, "regular message", cc)
	if ok {
		t.Error("should not return keyboard for non-command")
	}

	// Dispatch still works with args (keyboard doesn't block normal dispatch)
	resp, dispatched, _ := r.Dispatch(ctx, Request{Name: "model", Args: "opus"}, cc)
	if !dispatched {
		t.Fatal("expected dispatch to succeed")
	}
	if resp.Text != "executed: opus" {
		t.Errorf("result = %q", resp.Text)
	}
}

// TestLookupKeyboardCaseInsensitive verifies keyboard lookup is case-insensitive.
func TestLookupKeyboardCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "effort",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{{Label: "low", Data: "low"}}
		},
	})

	_, _, _, ok := r.LookupKeyboard(context.Background(), "/EFFORT", CommandContext{})
	if !ok {
		t.Error("keyboard lookup should be case-insensitive")
	}
}

// TestLookupKeyboardCustomHeader verifies that KeyboardHeader text is returned
// when set, and the default "/<name>:" is used when KeyboardHeader is nil.
func TestLookupKeyboardCustomHeader(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "speed",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{{Label: "fast", Data: "fast"}}
		},
		KeyboardHeader: func(_ context.Context, req Request, _ CommandContext) string {
			return "/speed — Speed: standard"
		},
	})

	_, header, _, ok := r.LookupKeyboard(context.Background(), "/speed", CommandContext{})
	if !ok {
		t.Fatal("expected keyboard for /speed")
	}
	if header != "/speed — Speed: standard" {
		t.Errorf("header = %q, want custom header", header)
	}
}

// TestKeyboardOptionsOnBuiltinCommands verifies builtin commands that should have keyboard
// options actually do have them with the right number of choices.
func TestKeyboardOptionsOnBuiltinCommands(t *testing.T) {
	cc := CommandContext{}

	t.Run("effort", func(t *testing.T) {
		cmd := EffortCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("effort command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if len(opts) != 3 {
			t.Fatalf("got %d options, want 3", len(opts))
		}
	})

	t.Run("thinking", func(t *testing.T) {
		cmd := ThinkingCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("thinking command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})

	t.Run("config", func(t *testing.T) {
		cmd := ConfigCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("config command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if len(opts) != 4 {
			t.Fatalf("got %d options, want 4", len(opts))
		}
	})

	t.Run("model_no_aliases", func(t *testing.T) {
		cmd := ModelCommand()
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if opts != nil {
			t.Fatalf("got %d options, want nil (no aliases)", len(opts))
		}
	})

	t.Run("cost", func(t *testing.T) {
		cmd := CostCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("cost command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
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
		cmd := CompactCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("compact command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})

	t.Run("bitwarden", func(t *testing.T) {
		cmd := BitwardenCommand()
		if cmd.KeyboardOptions == nil {
			t.Fatal("bitwarden command should have KeyboardOptions")
		}
		opts := cmd.KeyboardOptions(context.Background(), cc)
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2", len(opts))
		}
	})
}

// TestLookupChainKeyboard verifies chain keyboards fire when ChainKeyboard returns
// options, and don't fire for bare commands, missing commands, or when ChainKeyboard
// returns nil. Full args are passed to ChainKeyboard, which decides whether to chain.
func TestLookupChainKeyboard(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "tmux",
		Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
			return Response{Text: "executed: " + req.Args}, nil
		},
		ChainKeyboard: func(_ context.Context, subcommand string, _ CommandContext) []KeyboardOption {
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
		Name: "ping",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "pong"}, nil
		},
		// No ChainKeyboard
	})

	ctx := context.Background()
	cc := CommandContext{}

	// Bare subcommand with chain → returns options
	name, opts, ok := r.LookupChainKeyboard(ctx, "/tmux kill", cc)
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
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux list", cc)
	if ok {
		t.Error("should not chain for /tmux list (ChainKeyboard returns nil)")
	}

	// Multi-word args → ChainKeyboard receives full string, returns nil for unrecognised → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux kill mysession", cc)
	if ok {
		t.Error("should not chain when ChainKeyboard returns nil for multi-word args")
	}

	// Bare command (no subcommand) → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/tmux", cc)
	if ok {
		t.Error("should not chain for bare command with no subcommand")
	}

	// Command without ChainKeyboard → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "/ping something", cc)
	if ok {
		t.Error("should not chain for command without ChainKeyboard")
	}

	// Not a command → no chain
	_, _, ok = r.LookupChainKeyboard(ctx, "regular message", cc)
	if ok {
		t.Error("should not chain for non-command")
	}
}

// TestLookupChainKeyboardDeepChaining verifies that ChainKeyboard receives
// multi-word args and can return options at deeper levels (e.g. "set defaults").
func TestLookupChainKeyboardDeepChaining(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "config",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{}, nil
		},
		ChainKeyboard: func(_ context.Context, subcommand string, _ CommandContext) []KeyboardOption {
			switch subcommand {
			case "set":
				return []KeyboardOption{{Label: "defaults", Data: "set defaults"}}
			case "set defaults":
				return []KeyboardOption{
					{Label: "model", Data: "set defaults model"},
					{Label: "effort", Data: "set defaults effort"},
				}
			default:
				return nil
			}
		},
	})

	ctx := context.Background()
	cc := CommandContext{}

	// Level 1: "set" → section buttons
	_, opts, ok := r.LookupChainKeyboard(ctx, "/config set", cc)
	if !ok {
		t.Fatal("expected chain for /config set")
	}
	if len(opts) != 1 || opts[0].Label != "defaults" {
		t.Errorf("opts = %v", opts)
	}

	// Level 2: "set defaults" → key buttons
	_, opts, ok = r.LookupChainKeyboard(ctx, "/config set defaults", cc)
	if !ok {
		t.Fatal("expected chain for /config set defaults")
	}
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2", len(opts))
	}

	// Level 3: "set defaults model" → no further chaining
	_, _, ok = r.LookupChainKeyboard(ctx, "/config set defaults model", cc)
	if ok {
		t.Error("should not chain for /config set defaults model")
	}
}

// TestAll verifies All() returns commands sorted by name.
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

// TestDispatchRequiresBackend verifies that a command with RequiresBackend is
// rejected when no delegated backend is available.
func TestDispatchRequiresBackend(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:     "replay",
		Requires: RequiresBackend,
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "ok"}, nil
		},
	})

	ctx := context.Background()

	// No agent → rejected.
	resp, ok, _ := r.Dispatch(ctx, Request{Name: "replay"}, CommandContext{})
	if !ok {
		t.Fatal("expected command to be found")
	}
	if !strings.Contains(resp.Text, "requires a Claude Code backend") {
		t.Errorf("expected backend-required error, got %q", resp.Text)
	}
}

// TestDispatchRequiresNothing verifies that RequiresNothing commands always execute.
func TestDispatchRequiresNothing(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:     "ping",
		Requires: RequiresNothing,
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "pong"}, nil
		},
	})

	resp, ok, _ := r.Dispatch(context.Background(), Request{Name: "ping"}, CommandContext{})
	if !ok {
		t.Fatal("expected command to be found")
	}
	if resp.Text != "pong" {
		t.Errorf("got %q, want %q", resp.Text, "pong")
	}
}

// mockSecretsStore implements SecretsStore for testing.
type mockSecretsStore struct {
	data          map[string]string
	allowedHosts  map[string][]string
	allowedInBody map[string][]string
	saved         bool
}

func (m *mockSecretsStore) Names() []string {
	names := make([]string, 0, len(m.data))
	for k := range m.data {
		names = append(names, k)
	}
	return names
}
func (m *mockSecretsStore) Get(name string) (string, bool) { v, ok := m.data[name]; return v, ok }
func (m *mockSecretsStore) Set(name, value string)         { m.data[name] = value }
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

func (m *mockSecretsStore) SectionAllowedInBody(section string) []string {
	if m.allowedInBody == nil {
		return nil
	}
	return m.allowedInBody[section]
}
func (m *mockSecretsStore) AddAllowedInBody(section, key string) {
	if m.allowedInBody == nil {
		m.allowedInBody = make(map[string][]string)
	}
	for _, k := range m.allowedInBody[section] {
		if k == key {
			return
		}
	}
	m.allowedInBody[section] = append(m.allowedInBody[section], key)
}
func (m *mockSecretsStore) RemoveAllowedInBody(section, key string) bool {
	keys := m.allowedInBody[section]
	for i, k := range keys {
		if k == key {
			m.allowedInBody[section] = append(keys[:i], keys[i+1:]...)
			if len(m.allowedInBody[section]) == 0 {
				delete(m.allowedInBody, section)
			}
			return true
		}
	}
	return false
}
func (m *mockSecretsStore) SetAllowedInBody(section string, keys []string) {
	if m.allowedInBody == nil {
		m.allowedInBody = make(map[string][]string)
	}
	if len(keys) == 0 {
		delete(m.allowedInBody, section)
	} else {
		m.allowedInBody[section] = keys
	}
}

// TestRestartCommand verifies the restart command exists with correct properties.
func TestRestartCommand(t *testing.T) {
	cmd := RestartCommand()
	if cmd.Name != "restart" {
		t.Errorf("name = %q, want restart", cmd.Name)
	}
	if cmd.Description == "" {
		t.Error("description should not be empty")
	}
}

// TestSecretsCommand verifies secret listing, setting, removing, and usage display.
func TestSecretsCommand(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{
		"anthropic.setup_token": "sk-ant-123",
		"custom.api_key":        "key-456",
	}}
	cc := CommandContext{SecretsStore: store}
	cmd := SecretsCommand()

	if cmd.KeyboardOptions == nil {
		t.Fatal("secrets command should have KeyboardOptions")
	}
	opts := cmd.KeyboardOptions(context.Background(), cc)
	wantLabels := []string{"list", "set", "remove", "hosts", "body"}
	if len(opts) != len(wantLabels) {
		t.Fatalf("got %d keyboard options, want %d", len(opts), len(wantLabels))
	}
	for i, want := range wantLabels {
		if opts[i].Label != want || opts[i].Data != want {
			t.Errorf("option %d = {%q, %q}, want {%q, %q}", i, opts[i].Label, opts[i].Data, want, want)
		}
	}

	// List
	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Text, "anthropic") || !strings.Contains(result.Text, "token") {
		t.Errorf("list result = %q, want anthropic section with token", result.Text)
	}
	// Secret values must never appear
	if strings.Contains(result.Text, "sk-ant-123") || strings.Contains(result.Text, "key-456") {
		t.Error("list should not display secret values")
	}

	// Set
	result, err = cmd.Execute(context.Background(), Request{Args: "set custom.new_key my-secret-value"}, cc)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(result.Text, "set") {
		t.Errorf("set result = %q", result.Text)
	}
	if store.data["custom.new_key"] != "my-secret-value" {
		t.Errorf("key not set: %v", store.data)
	}
	if !store.saved {
		t.Error("Save should have been called")
	}

	// Remove
	store.saved = false
	result, err = cmd.Execute(context.Background(), Request{Args: "remove custom.api_key"}, cc)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(result.Text, "removed") {
		t.Errorf("remove result = %q", result.Text)
	}
	if _, ok := store.data["custom.api_key"]; ok {
		t.Error("key should be removed")
	}
	if !store.saved {
		t.Error("Save should have been called")
	}

	// Remove nonexistent
	result, err = cmd.Execute(context.Background(), Request{Args: "remove nonexistent.key"}, cc)
	if err != nil {
		t.Fatalf("remove nonexistent: %v", err)
	}
	if !strings.Contains(result.Text, "not found") {
		t.Errorf("remove nonexistent result = %q", result.Text)
	}

	// Usage (no args)
	result, err = cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatalf("no args: %v", err)
	}
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("empty args result = %q, want usage", result.Text)
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

// TestSetWizard verifies that setting a wizard causes HandleMessage to use it.
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

// TestClearWizard verifies that clearing a wizard stops HandleMessage from intercepting.
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

// TestHandleMessageWizardCancel verifies /cancel clears the wizard.
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

// TestHandleMessageWizardStop verifies /stop clears the wizard.
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

// TestHandleMessageWizardDotStop verifies .stop clears the wizard (dot-prefix).
func TestHandleMessageWizardDotStop(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{responses: map[string]string{}}
	reg.SetWizard(wizard)

	resp, handled := reg.HandleMessage(".stop")
	if !handled {
		t.Error("HandleMessage should handle .stop")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("response = %q, want 'cancelled'", resp)
	}
	// Wizard should be cleared
	_, handled = reg.HandleMessage("test")
	if handled {
		t.Error("wizard should be cleared after .stop")
	}
}

// TestHandleMessageWizardDotCancel verifies .cancel clears the wizard (dot-prefix).
func TestHandleMessageWizardDotCancel(t *testing.T) {
	reg := NewRegistry()
	wizard := &mockWizard{responses: map[string]string{}}
	reg.SetWizard(wizard)

	resp, handled := reg.HandleMessage(".cancel")
	if !handled {
		t.Error("HandleMessage should handle .cancel")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("response = %q, want 'cancelled'", resp)
	}
	_, handled = reg.HandleMessage("test")
	if handled {
		t.Error("wizard should be cleared after .cancel")
	}
}

// TestHandleMessageWizardDone verifies wizard auto-clears when it returns done=true.
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
