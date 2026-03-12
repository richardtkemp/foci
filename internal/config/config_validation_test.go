package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCompactionThreshold(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"threshold too high",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = 1.5",
			"compaction_threshold = 1.5",
		},
		{
			"threshold negative",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = -0.1",
			"compaction_threshold = -0.1",
		},
		{
			"threshold valid",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = 0.7",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateHTTPPort(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"port too high",
			"[agent]\nid = \"test\"\n[http]\nport = 70000",
			"port = 70000",
		},
		{
			"port zero",
			// port 0 gets defaulted to 18791, so it should pass
			"[agent]\nid = \"test\"\n[http]\nport = 0",
			"",
		},
		{
			"port valid",
			"[agent]\nid = \"test\"\n[http]\nport = 8080",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateLoggingLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[logging]\nlevel = \"BOGUS\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid logging level")
	}
	if !strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("error = %q, want mention of BOGUS", err.Error())
	}
}

func TestValidateCacheStrategy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[cache]\nstrategy = \"invalid\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid cache strategy")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want mention of invalid", err.Error())
	}
}

func TestValidateCacheTTL(t *testing.T) {
	// Verify that invalid cache TTL values are rejected during config validation.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[cache]\nttl = \"30m\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid cache TTL")
	}
	if !strings.Contains(err.Error(), "ttl") {
		t.Errorf("error = %q, want mention of ttl", err.Error())
	}
}

func TestValidateCacheTTLValid(t *testing.T) {
	// Verify valid cache TTL values are accepted.
	dir := t.TempDir()
	for _, ttl := range []string{"5m", "1h"} {
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(fmt.Sprintf("[agent]\nid = \"test\"\n[cache]\nttl = %q", ttl)), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Errorf("ttl=%q: unexpected error: %v", ttl, err)
		}
		if cfg.Cache.TTL != ttl {
			t.Errorf("ttl=%q: got %q", ttl, cfg.Cache.TTL)
		}
	}
}

func TestValidateWarningWindowDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[logging]\nwarning_window_duration = \"bogus\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid warning_window_duration")
	}
	if !strings.Contains(err.Error(), "warning_window_duration") {
		t.Errorf("error = %q, want mention of warning_window_duration", err.Error())
	}
}

func TestValidateMemorySourceWeight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[[memory.sources]]
name = "bad"
dir = "/tmp"
weight = 2.0
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for weight > 1.0")
	}
	if !strings.Contains(err.Error(), "weight") {
		t.Errorf("error = %q, want mention of weight", err.Error())
	}
}

func TestLoadMemoryConversationWeightDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Memory.ConversationWeight != 0.1 {
		t.Errorf("ConversationWeight = %f, want default 0.1", cfg.Memory.ConversationWeight)
	}
}

func TestLoadMemoryConversationWeightCustom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[memory]
conversation_weight = 0.25
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Memory.ConversationWeight != 0.25 {
		t.Errorf("ConversationWeight = %f, want 0.25", cfg.Memory.ConversationWeight)
	}
}

func TestValidateMemoryConversationWeight(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"weight too high",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = 1.5",
			"conversation_weight = 1.5",
		},
		{
			"weight negative",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = -0.1",
			"conversation_weight = -0.1",
		},
		{
			"weight valid",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = 0.5",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateNewDurationFields(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "invalid http_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
http_timeout = "invalid"
`,
			wantErr: "http_timeout",
		},
		{
			name: "invalid database busy_timeout",
			toml: `
[agent]
id = "test"
[database]
busy_timeout = "invalid"
`,
			wantErr: "busy_timeout",
		},
		{
			name: "invalid telegram long_poll_timeout",
			toml: `
[agent]
id = "test"
[telegram]
long_poll_timeout = "invalid"
`,
			wantErr: "long_poll_timeout",
		},
		{
			name: "invalid http graceful_shutdown_timeout",
			toml: `
[agent]
id = "test"
[http]
graceful_shutdown_timeout = "invalid"
`,
			wantErr: "graceful_shutdown_timeout",
		},
		{
			name: "invalid tools tmux_command_timeout",
			toml: `
[agent]
id = "test"
[tools]
tmux_command_timeout = "invalid"
`,
			wantErr: "tmux_command_timeout",
		},
		{
			name: "invalid tools web_fetch_timeout",
			toml: `
[agent]
id = "test"
[tools]
web_fetch_timeout = "invalid"
`,
			wantErr: "web_fetch_timeout",
		},
		{
			name: "invalid tools web_search_timeout",
			toml: `
[agent]
id = "test"
[tools]
web_search_timeout = "invalid"
`,
			wantErr: "web_search_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateReservedAgentIDs(t *testing.T) {
	// Verifies that agent IDs matching reserved home directory names are rejected,
	// and that dot-prefixed IDs are rejected, while normal IDs pass.
	reserved := []string{"bin", "character", "config", "data", "go", "logs", "memory", "oldscripts", "scripts", "shared"}
	for _, id := range reserved {
		t.Run("reserved_"+id, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(fmt.Sprintf("[agent]\nid = %q", id)), 0644)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for reserved agent id %q", id)
			}
			if !strings.Contains(err.Error(), "reserved directory") {
				t.Errorf("error = %q, want mention of reserved directory", err.Error())
			}
		})
	}

	// Dot-prefixed IDs
	for _, id := range []string{".hidden", ".config", "."} {
		t.Run("dot_"+id, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(fmt.Sprintf("[agent]\nid = %q", id)), 0644)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for dot-prefixed agent id %q", id)
			}
			if !strings.Contains(err.Error(), "dot") {
				t.Errorf("error = %q, want mention of dot", err.Error())
			}
		})
	}

	// Valid IDs should pass
	for _, id := range []string{"clutch", "myagent", "test123"} {
		t.Run("valid_"+id, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(fmt.Sprintf("[agent]\nid = %q", id)), 0644)

			_, err := Load(path)
			if err != nil {
				t.Fatalf("unexpected error for valid agent id %q: %v", id, err)
			}
		})
	}
}

func TestValidateMemoryThreshold(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid percentage
		{"valid percent 50", "50%", false, ""},
		{"valid percent 1", "1%", false, ""},
		{"valid percent 100", "100%", false, ""},
		{"valid percent decimal", "50.5%", false, ""},
		{"valid percent with spaces", "  50%  ", false, ""},
		// Valid MB
		{"valid mb", "512mb", false, ""},
		{"valid mb decimal", "512.5mb", false, ""},
		{"valid mb uppercase", "512MB", false, ""},
		// Valid GB
		{"valid gb", "2gb", false, ""},
		{"valid gb decimal", "2.5gb", false, ""},
		{"valid gb uppercase", "2GB", false, ""},
		// Invalid
		{"empty string", "", true, "empty"},
		{"invalid percent 0", "0%", true, "between 0 and 100"},
		{"invalid percent 101", "101%", true, "between 0 and 100"},
		{"invalid percent negative", "-50%", true, "between 0 and 100"},
		{"invalid percent not number", "abc%", true, "invalid percentage"},
		{"invalid mb 0", "0mb", true, "must be positive"},
		{"invalid mb negative", "-512mb", true, "must be positive"},
		{"invalid mb not number", "abcmb", true, "invalid megabytes"},
		{"invalid gb 0", "0gb", true, "must be positive"},
		{"invalid gb negative", "-2gb", true, "must be positive"},
		{"invalid gb not number", "abcgb", true, "invalid gigabytes"},
		{"invalid format kb", "512kb", true, "unknown format"},
		{"invalid format plain number", "512", true, "unknown format"},
		{"invalid format no unit", "512", true, "unknown format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMemoryThreshold(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMemoryThreshold(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateMemoryThreshold(%q) error = %q, want to contain %q", tt.input, err.Error(), tt.errMsg)
			}
		})
	}
}
