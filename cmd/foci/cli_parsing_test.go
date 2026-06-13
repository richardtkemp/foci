package main

import (
	"testing"
)

// TestParseSendFlags tests basic flag parsing with agent, session, and timing flags.
func TestParseSendFlags(t *testing.T) {
	tests := []struct {
		name               string
		args               []string
		wantAgent          string
		wantSession        string
		wantIfActive       string
		wantIfInactive     string
		wantIfUserActive   string
		wantIfUserInactive string
		wantRest           []string
	}{
		{
			name:     "no flags",
			args:     []string{"hello", "world"},
			wantRest: []string{"hello", "world"},
		},
		{
			name:         "--if-active with value",
			args:         []string{"--if-active", "8h", "hello"},
			wantIfActive: "8h",
			wantRest:     []string{"hello"},
		},
		{
			name:         "--if-active=value",
			args:         []string{"--if-active=30m", "hello"},
			wantIfActive: "30m",
			wantRest:     []string{"hello"},
		},
		{
			name:         "all flags together",
			args:         []string{"-a", "clutch", "-s", "main", "--if-active", "4h", "hello"},
			wantAgent:    "clutch",
			wantSession:  "main",
			wantIfActive: "4h",
			wantRest:     []string{"hello"},
		},
		{
			name:         "--if-active after text",
			args:         []string{"hello", "--if-active", "12h"},
			wantIfActive: "12h",
			wantRest:     []string{"hello"},
		},
		{
			name:     "--if-active without value at end",
			args:     []string{"hello", "--if-active"},
			wantRest: []string{"hello", "--if-active"},
		},
		{
			name:           "--if-inactive with value",
			args:           []string{"--if-inactive", "30m", "hello"},
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		{
			name:           "--if-inactive=value",
			args:           []string{"--if-inactive=1h", "hello"},
			wantIfInactive: "1h",
			wantRest:       []string{"hello"},
		},
		{
			name:           "both --if-active and --if-inactive",
			args:           []string{"--if-active", "8h", "--if-inactive", "30m", "hello"},
			wantIfActive:   "8h",
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		// --- TODO #753: user-attention gates ---
		{
			name:             "--if-user-active with value",
			args:             []string{"--if-user-active", "2h", "hello"},
			wantIfUserActive: "2h",
			wantRest:         []string{"hello"},
		},
		{
			name:             "--if-user-active=value",
			args:             []string{"--if-user-active=45m", "hello"},
			wantIfUserActive: "45m",
			wantRest:         []string{"hello"},
		},
		{
			name:               "--if-user-inactive with value",
			args:               []string{"--if-user-inactive", "30m", "hello"},
			wantIfUserInactive: "30m",
			wantRest:           []string{"hello"},
		},
		{
			name:               "--if-user-inactive=value",
			args:               []string{"--if-user-inactive=1h", "hello"},
			wantIfUserInactive: "1h",
			wantRest:           []string{"hello"},
		},
		{
			name:               "all four gate flags coexist",
			args:               []string{"--if-active", "8h", "--if-inactive", "30m", "--if-user-active", "2h", "--if-user-inactive", "10m", "hello"},
			wantIfActive:       "8h",
			wantIfInactive:     "30m",
			wantIfUserActive:   "2h",
			wantIfUserInactive: "10m",
			wantRest:           []string{"hello"},
		},
		{
			name:     "--if-user-active without value at end",
			args:     []string{"hello", "--if-user-active"},
			wantRest: []string{"hello", "--if-user-active"},
		},
		{
			name:     "--if-user-inactive without value at end",
			args:     []string{"hello", "--if-user-inactive"},
			wantRest: []string{"hello", "--if-user-inactive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", flags.agent, tt.wantAgent)
			}
			if flags.session != tt.wantSession {
				t.Errorf("session = %q, want %q", flags.session, tt.wantSession)
			}
			if flags.ifActive != tt.wantIfActive {
				t.Errorf("ifActive = %q, want %q", flags.ifActive, tt.wantIfActive)
			}
			if flags.ifInactive != tt.wantIfInactive {
				t.Errorf("ifInactive = %q, want %q", flags.ifInactive, tt.wantIfInactive)
			}
			if flags.ifUserActive != tt.wantIfUserActive {
				t.Errorf("ifUserActive = %q, want %q", flags.ifUserActive, tt.wantIfUserActive)
			}
			if flags.ifUserInactive != tt.wantIfUserInactive {
				t.Errorf("ifUserInactive = %q, want %q", flags.ifUserInactive, tt.wantIfUserInactive)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

// TestParseSendFlagsAsyncSync tests --async, --sync, --wait, and --no-wait flag parsing.
func TestParseSendFlagsAsyncSync(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAsync bool
		wantSync  bool
		wantRest  []string
	}{
		{"--async", []string{"--async", "hello"}, true, false, []string{"hello"}},
		{"--no-wait", []string{"--no-wait", "hello"}, true, false, []string{"hello"}},
		{"--sync", []string{"--sync", "hello"}, false, true, []string{"hello"}},
		{"--wait", []string{"--wait", "hello"}, false, true, []string{"hello"}},
		{"--sync with other flags", []string{"-a", "clutch", "--sync", "hello"}, false, true, []string{"hello"}},
		{"no async/sync flags", []string{"hello"}, false, false, []string{"hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.async != tt.wantAsync {
				t.Errorf("async = %v, want %v", flags.async, tt.wantAsync)
			}
			if flags.sync != tt.wantSync {
				t.Errorf("sync = %v, want %v", flags.sync, tt.wantSync)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

// TestParseSendFlagsMessageFlags tests -mt, -mf, --message-text, and --message-file flag parsing.
func TestParseSendFlagsMessageFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMT   string
		wantMF   string
		wantRest []string
	}{
		{"-mt with value", []string{"-mt", "hello"}, "hello", "", nil},
		{"--mt with value", []string{"--mt", "hello"}, "hello", "", nil},
		{"--message-text with value", []string{"--message-text", "hello"}, "hello", "", nil},
		{"-mt=value", []string{"-mt=hello"}, "hello", "", nil},
		{"--mt=value", []string{"--mt=hello"}, "hello", "", nil},
		{"--message-text=value", []string{"--message-text=hello"}, "hello", "", nil},
		{"-mf with value", []string{"-mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--mf with value", []string{"--mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file with value", []string{"--message-file", "/tmp/f"}, "", "/tmp/f", nil},
		{"-mf=value", []string{"-mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--mf=value", []string{"--mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file=value", []string{"--message-file=/tmp/f"}, "", "/tmp/f", nil},
		{"-mt with other flags", []string{"-a", "clutch", "-mt", "hi", "extra"}, "hi", "", []string{"extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.messageText != tt.wantMT {
				t.Errorf("messageText = %q, want %q", flags.messageText, tt.wantMT)
			}
			if flags.messageFile != tt.wantMF {
				t.Errorf("messageFile = %q, want %q", flags.messageFile, tt.wantMF)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

// TestParseAPIKeyFlag tests --api-key flag parsing in various formats.
func TestParseAPIKeyFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantKey  string
		wantRest []string
	}{
		{
			name:     "no flag",
			args:     []string{"send", "hello"},
			wantKey:  "",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key with value",
			args:     []string{"--api-key", "my-secret", "send", "hello"},
			wantKey:  "my-secret",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key=value",
			args:     []string{"--api-key=my-secret", "send", "hello"},
			wantKey:  "my-secret",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "flag in middle",
			args:     []string{"send", "--api-key", "key123", "hello"},
			wantKey:  "key123",
			wantRest: []string{"send", "hello"},
		},
		{
			name:     "--api-key without value at end",
			args:     []string{"send", "--api-key"},
			wantKey:  "",
			wantRest: []string{"send", "--api-key"},
		},
		{
			name:     "empty args",
			args:     []string{},
			wantKey:  "",
			wantRest: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, rest := parseAPIKeyFlag(tt.args)
			if key != tt.wantKey {
				t.Errorf("apiKey = %q, want %q", key, tt.wantKey)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

// TestParseAgentFlag tests -a and --agent flag parsing in various forms.
func TestParseAgentFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAgent string
		wantRest  []string
	}{
		{
			name:      "no flag",
			args:      []string{"hello", "world"},
			wantAgent: "",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "-a with value",
			args:      []string{"-a", "research", "hello"},
			wantAgent: "research",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent with value",
			args:      []string{"--agent", "main", "hello"},
			wantAgent: "main",
			wantRest:  []string{"hello"},
		},
		{
			name:      "-a=value",
			args:      []string{"-a=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent=value",
			args:      []string{"--agent=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "flag after positional args",
			args:      []string{"hello", "world", "-a", "research"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "flag in middle",
			args:      []string{"hello", "--agent", "research", "world"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "empty args",
			args:      []string{},
			wantAgent: "",
			wantRest:  []string{},
		},
		{
			name:      "-a without value at end",
			args:      []string{"hello", "-a"},
			wantAgent: "",
			wantRest:  []string{"hello", "-a"},
		},
		{
			name:      "only -a and value",
			args:      []string{"-a", "research"},
			wantAgent: "research",
			wantRest:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, rest := parseAgentFlag(tt.args)
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return // both empty, ok
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}
