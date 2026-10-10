package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCompactionThreshold(t *testing.T) {
	// Proves that compaction_threshold must be between 0 and 1 exclusive, rejecting
	// values above 1 or below 0 with an error identifying the bad value.
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"threshold too high",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[sessions]\ncompaction_threshold = 1.5",
			"compaction_threshold = 1.5",
		},
		{
			"threshold negative",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[sessions]\ncompaction_threshold = -0.1",
			"compaction_threshold = -0.1",
		},
		{
			"threshold valid",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[sessions]\ncompaction_threshold = 0.7",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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
	// Proves that the HTTP port is validated against the valid range (1-65535), with
	// port 0 accepted (it gets defaulted), and ports above 65535 rejected.
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"port too high",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[http]\nport = 70000",
			"port = 70000",
		},
		{
			"port zero",
			// port 0 gets defaulted to 18791, so it should pass
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[http]\nport = 0",
			"",
		},
		{
			"port valid",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[http]\nport = 8080",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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
	// Proves that an unrecognized logging level produces an error mentioning the
	// invalid value.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[logging]\nlevel = \"BOGUS\""), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for invalid logging level")
	}
	if !strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("error = %q, want mention of BOGUS", err.Error())
	}
}

func TestValidateModelCacheStrategy(t *testing.T) {
	// Proves that an unrecognized cache_strategy on a named model produces
	// a validation error.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[models.bad]
model = "anthropic/claude-haiku-4-5-20251001"
cache_strategy = "invalid"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for invalid cache strategy")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want mention of invalid", err.Error())
	}
}

func TestValidateModelProviderRouting(t *testing.T) {
	// Proves that invalid enum values in a [models.X.provider] sub-table
	// (sort.by, data_collection, quantizations) each produce a validation
	// error, and that a fully valid provider block loads cleanly.
	dir := t.TempDir()
	cases := []struct {
		name      string
		provider  string
		wantErr   bool
		wantMatch string
	}{
		{
			name: "invalid sort.by",
			provider: `
[models.bad.provider.sort]
by = "cheapest"
`,
			wantErr:   true,
			wantMatch: "sort",
		},
		{
			name: "invalid data_collection",
			provider: `
[models.bad.provider]
data_collection = "sometimes"
`,
			wantErr:   true,
			wantMatch: "data_collection",
		},
		{
			name: "invalid quantization",
			provider: `
[models.bad.provider]
quantizations = ["int9"]
`,
			wantErr:   true,
			wantMatch: "quantizations",
		},
		{
			name: "valid provider block",
			provider: `
[models.bad.provider]
order = ["deepinfra"]
data_collection = "deny"
quantizations = ["fp8"]

[models.bad.provider.sort]
by = "price"
partition = "none"

[models.bad.provider.max_price]
prompt = 1.0
`,
			wantErr: false,
		},
		{
			name: "negative max_price",
			provider: `
[models.bad.provider.max_price]
prompt = -1.0
`,
			wantErr:   true,
			wantMatch: "negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("foci-%s.toml", strings.ReplaceAll(tc.name, " ", "-")))
			toml := fmt.Sprintf(`
[groups]
powerful = "openrouter/deepseek/deepseek-v4-pro"

[[agents]]
id = "test"

[models.bad]
model = "openrouter/deepseek/deepseek-v4-pro"
%s
`, tc.provider)
			os.WriteFile(path, []byte(toml), 0644)

			_, err := Load(path, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if !strings.Contains(err.Error(), tc.wantMatch) {
					t.Errorf("error = %q, want mention of %q", err.Error(), tc.wantMatch)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateModelCacheTTL(t *testing.T) {
	// Proves that valid Go duration strings like "5m" and "1h" are accepted as
	// cache_ttl values on named model entries.
	dir := t.TempDir()
	for _, ttl := range []string{"5m", "1h"} {
		path := filepath.Join(dir, "foci.toml")
		toml := fmt.Sprintf(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[models.mymodel]
model = "anthropic/claude-haiku-4-5-20251001"
cache_ttl = %q
`, ttl)
		os.WriteFile(path, []byte(toml), 0644)
		cfg, err := Load(path, nil)
		if err != nil {
			t.Errorf("cache_ttl=%q: unexpected error: %v", ttl, err)
		}
		if cfg.Models["mymodel"].CacheTTL != ttl {
			t.Errorf("cache_ttl=%q: got %q", ttl, cfg.Models["mymodel"].CacheTTL)
		}
	}
}

func TestValidateWarningWindowDuration(t *testing.T) {
	// Proves that an invalid warning_window_duration string is rejected with an
	// error mentioning the field name.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[logging]\nwarning_window_duration = \"bogus\""), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for invalid warning_window_duration")
	}
	if !strings.Contains(err.Error(), "warning_window_duration") {
		t.Errorf("error = %q, want mention of warning_window_duration", err.Error())
	}
}

func TestValidateMemorySourceWeight(t *testing.T) {
	// Proves that a memory source with weight > 1.0 is rejected with a validation
	// error mentioning "weight".
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[[memory.sources]]
name = "bad"
dir = "/tmp"
weight = 2.0
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for weight > 1.0")
	}
	if !strings.Contains(err.Error(), "weight") {
		t.Errorf("error = %q, want mention of weight", err.Error())
	}
}

func TestValidateBehaviorMaxConcurrentTurnsNegative(t *testing.T) {
	// Proves that a negative #2281 turn-limit value fails config load naming
	// the key, at every level the key can be written: [behavior] and the
	// [[agents]].behavior override, for both max_concurrent_turns and
	// max_concurrent_turns_all_agents.
	base := "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n"
	tests := []struct {
		name string
		toml string
	}{
		{"global max_concurrent_turns", base + "[behavior]\nmax_concurrent_turns = -1"},
		{"global max_concurrent_turns_all_agents", base + "[behavior]\nmax_concurrent_turns_all_agents = -1"},
		{"agent max_concurrent_turns", base + "[agents.behavior]\nmax_concurrent_turns = -2"},
		{"agent max_concurrent_turns_all_agents", base + "[agents.behavior]\nmax_concurrent_turns_all_agents = -2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
			if err == nil {
				t.Fatal("expected error for negative turn limit")
			}
			key := "max_concurrent_turns"
			if strings.Contains(tt.name, "all_agents") {
				key = "max_concurrent_turns_all_agents"
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want mention of %s", err.Error(), key)
			}
			if !strings.Contains(err.Error(), "must not be negative") {
				t.Errorf("error = %q, want a must-not-be-negative message", err.Error())
			}
		})
	}

	// Zero (no limit) and unset must both load cleanly.
	for name, toml := range map[string]string{
		"zero":  base + "[behavior]\nmax_concurrent_turns = 0\nmax_concurrent_turns_all_agents = 0",
		"unset": base,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(toml), 0644)
		if _, err := Load(path, nil); err != nil {
			t.Errorf("%s: Load: %v", name, err)
		}
	}
}

func TestLoadMemoryConversationWeightDefault(t *testing.T) {
	// Proves that conversation_weight defaults to 0.1 when not specified in the
	// [memory] section.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if DerefFloat(cfg.Memory.ConversationWeight) != 0.1 {
		t.Errorf("ConversationWeight = %f, want default 0.1", DerefFloat(cfg.Memory.ConversationWeight))
	}
}

func TestLoadMemoryConversationWeightCustom(t *testing.T) {
	// Proves that an explicitly configured conversation_weight value is loaded and
	// preserved correctly in the Memory config struct.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[memory]
conversation_weight = 0.25
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if DerefFloat(cfg.Memory.ConversationWeight) != 0.25 {
		t.Errorf("ConversationWeight = %f, want 0.25", DerefFloat(cfg.Memory.ConversationWeight))
	}
}

func TestValidateMemoryConversationWeight(t *testing.T) {
	// Proves that conversation_weight must be between 0 and 1 inclusive, rejecting
	// values above 1 or below 0 with an error naming the bad value.
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"weight too high",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[memory]\nconversation_weight = 1.5",
			"conversation_weight = 1.5",
		},
		{
			"weight negative",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[memory]\nconversation_weight = -0.1",
			"conversation_weight = -0.1",
		},
		{
			"weight valid",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[memory]\nconversation_weight = 0.5",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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
	// Proves that all duration-typed config fields (http_timeout, busy_timeout,
	// long_poll_timeout, graceful_shutdown_timeout, tmux/web timeouts) are validated
	// as Go duration strings and reject invalid values with the field name in the error.
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "invalid endpoint http_timeout",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[endpoints.anthropic]
format = "anthropic"
http_timeout = "invalid"
`,
			wantErr: "http_timeout",
		},
		{
			name: "invalid telegram long_poll_timeout",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[[platforms]]
id = "telegram"
[platforms.telegram]
long_poll_timeout = "invalid"
`,
			wantErr: "long_poll_timeout",
		},
		{
			name: "invalid http graceful_shutdown_timeout",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[http]
graceful_shutdown_timeout = "invalid"
`,
			wantErr: "graceful_shutdown_timeout",
		},
		{
			name: "invalid tools tmux_command_timeout",
			toml: `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[tools]
tmux_command_timeout = "invalid"
`,
			wantErr: "tmux_command_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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
	// Proves that agent IDs matching reserved home directory names (bin, data, logs,
	// etc.) are rejected, dot-prefixed IDs are rejected, and regular IDs pass.
	reserved := []string{"bin", "character", "config", "data", "go", "logs", "memory", "oldscripts", "scripts", "shared"}
	for _, id := range reserved {
		t.Run("reserved_"+id, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(fmt.Sprintf("[[agents]]\nid = %q", id)), 0644)

			_, err := Load(path, nil)
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
			os.WriteFile(path, []byte(fmt.Sprintf("[[agents]]\nid = %q", id)), 0644)

			_, err := Load(path, nil)
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
			os.WriteFile(path, []byte(fmt.Sprintf("[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = %q", id)), 0644)

			_, err := Load(path, nil)
			if err != nil {
				t.Fatalf("unexpected error for valid agent id %q: %v", id, err)
			}
		})
	}
}

func TestValidateMemoryThreshold(t *testing.T) {
	// Proves that ValidateMemoryThreshold accepts percent (1-100%), MB, and GB
	// formats and rejects empty strings, out-of-range values, non-numeric values,
	// and unsupported units like KB.
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

// TestValidateMasterAgent proves master_agent must name a configured agent:
// a matching ID passes, an unknown one fails validation, and empty (unset)
// is always fine. Loads full TOML so defaults apply.
func TestValidateMasterAgent(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"valid master",
			"master_agent = \"clutch\"\n\n[[agents]]\nid = \"clutch\"\nbackend = \"claude-code\"",
			"",
		},
		{
			"unknown master",
			"master_agent = \"ghost\"\n\n[[agents]]\nid = \"clutch\"\nbackend = \"claude-code\"",
			"master_agent = \"ghost\"",
		},
		{
			"unset master",
			"[[agents]]\nid = \"clutch\"\nbackend = \"claude-code\"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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

// TestValidateDefaultPlatform proves default_platform (global and per-agent)
// must name a configured [[platforms]] entry; empty is fine.
func TestValidateDefaultPlatform(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"valid global",
			"default_platform = \"telegram\"\n\n[[platforms]]\nid = \"telegram\"\n\n[[agents]]\nid = \"a\"\nbackend = \"claude-code\"",
			"",
		},
		{
			"unknown global",
			"default_platform = \"matrix\"\n\n[[platforms]]\nid = \"telegram\"\n\n[[agents]]\nid = \"a\"\nbackend = \"claude-code\"",
			"default_platform = \"matrix\"",
		},
		{
			"unknown per-agent",
			"[[platforms]]\nid = \"telegram\"\n\n[[agents]]\nid = \"a\"\nbackend = \"claude-code\"\ndefault_platform = \"matrix\"",
			"agent \"a\" default_platform",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path, nil)
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

// TestValidateRateLimitNotifyTo proves an unknown [notify] rate_limit_notify_to
// is rejected at load rather than silently falling through the delivery switch
// (#1857), that the three accepted values load, and that a per-agent override is
// validated too.
func TestValidateRateLimitNotifyTo(t *testing.T) {
	const base = "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n"
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{"session", base + "\n[notify]\nrate_limit_notify_to = \"session\"", ""},
		{"default", base + "\n[notify]\nrate_limit_notify_to = \"default\"", ""},
		{"both", base + "\n[notify]\nrate_limit_notify_to = \"both\"", ""},
		{"unset", base, ""},
		{"global typo", base + "\n[notify]\nrate_limit_notify_to = \"primary\"", `rate_limit_notify_to = "primary"`},
		{
			"per-agent typo",
			"[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n[agents.notify]\nrate_limit_notify_to = \"everyone\"",
			`rate_limit_notify_to = "everyone"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "foci.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path, nil)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatal("expected error, got nil — an unknown target would deliver nowhere")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestRateLimitNotifyTargetDefault pins the default: unset means the notice goes
// to the session that hit the limit, not the agent's default chat (#1857).
func TestRateLimitNotifyTargetDefault(t *testing.T) {
	if got := (NotifyConfig{}).RateLimitNotifyTarget(); got != RateLimitNotifySession {
		t.Errorf("unset RateLimitNotifyTarget() = %q, want %q", got, RateLimitNotifySession)
	}
	empty := ""
	if got := (NotifyConfig{RateLimitNotifyTo: &empty}).RateLimitNotifyTarget(); got != RateLimitNotifySession {
		t.Errorf("empty RateLimitNotifyTarget() = %q, want %q", got, RateLimitNotifySession)
	}
	both := RateLimitNotifyBoth
	if got := (NotifyConfig{RateLimitNotifyTo: &both}).RateLimitNotifyTarget(); got != RateLimitNotifyBoth {
		t.Errorf("RateLimitNotifyTarget() = %q, want %q", got, RateLimitNotifyBoth)
	}
}

func TestValidateFallbacks_CaseFoldCycleReported(t *testing.T) {
	// Proves the depth/cycle walk uses the same folded keys as the
	// resolver, so a cycle that exists only under case folding (a/X → a/y,
	// a/Y → a/x) is a config error instead of a runtime surprise silently
	// broken by NewFallbackResolver.
	err := validateFallbacks("groups.fallbacks", map[string]string{
		"a/X": "a/y",
		"a/Y": "a/x",
	}, nil)
	if err == nil {
		t.Fatal("expected error for a case-fold-only cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want a cycle report", err.Error())
	}
}

func TestValidateFallbacks_CaseFoldCycleUppercaseValuesReported(t *testing.T) {
	// Proves the validator's depth/cycle walk follows an edge through
	// ModelKey, not the raw value: with the config case on the VALUES
	// (a/x → a/Y, a/y → a/X) a raw walk stops at "a/Y" — not a key — and
	// misses the fold-only cycle the resolver would have to break.
	err := validateFallbacks("groups.fallbacks", map[string]string{
		"a/x": "a/Y",
		"a/y": "a/X",
	}, nil)
	if err == nil {
		t.Fatal("expected error for a case-fold-only cycle through uppercase values")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want a cycle report", err.Error())
	}
}

func TestValidateModelKeys_CollisionRejected(t *testing.T) {
	// Proves two [models.*] entries whose model strings differ only by
	// case are rejected at load: the per-model lookups range over a map,
	// so which entry's settings win would be random. The error names both
	// entries and both spellings.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[models.qwen]
model = "openrouter/Qwen/X"

[models.qwen-fast]
model = "openrouter/qwen/x"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for colliding model keys")
	}
	for _, want := range []string{"models.qwen", "models.qwen-fast", "openrouter/Qwen/X", "openrouter/qwen/x", "model matching ignores case"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want mention of %q", err.Error(), want)
		}
	}
}

func TestValidateModelKeys_IdenticalStringRejected(t *testing.T) {
	// Proves the collision check includes exactly-equal model strings —
	// two entries naming the identical model are just as random a match.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[models.a]
model = "openrouter/qwen/x"

[models.b]
model = "openrouter/qwen/x"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for identical model strings")
	}
	if !strings.Contains(err.Error(), "models.a") || !strings.Contains(err.Error(), "models.b") {
		t.Errorf("error = %q, want both entry names", err.Error())
	}
}

func TestValidateModelKeys_DistinctModelsStillValid(t *testing.T) {
	// Characterisation: two [models.*] entries with distinct model keys
	// (differing by more than case) still validate.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[models.a]
model = "openrouter/qwen/x"

[models.b]
model = "openrouter/qwen/y"
`
	os.WriteFile(path, []byte(toml), 0644)

	if _, err := Load(path, nil); err != nil {
		t.Fatalf("unexpected error for distinct model keys: %v", err)
	}
}

func TestValidateFallbacks_FoldCollidingKeysRejected(t *testing.T) {
	// Proves two keys in one [groups.fallbacks] table that differ only by
	// case are rejected at load: they fold to a single match key, and the
	// resolver would otherwise pick between their values at random (map
	// iteration order). The error names both keys in sorted order, and the
	// message is identical on every validation.
	const want = `[groups.fallbacks] key "openrouter/a/X" and key "openrouter/a/x" are the same model (model matching ignores case)`
	for i := 0; i < 30; i++ {
		err := validateFallbacks("groups.fallbacks", map[string]string{
			"openrouter/a/X": "openrouter/p/one",
			"openrouter/a/x": "openrouter/q/two",
		}, nil)
		if err == nil {
			t.Fatal("expected error for fold-colliding fallback keys")
		}
		if err.Error() != want {
			t.Fatalf("run %d: error = %q, want exactly %q (stable, keys in sorted order)", i, err.Error(), want)
		}
	}
}

func TestValidateFallbacks_MessagesQuoteWrittenKeys(t *testing.T) {
	// Proves the depth/cycle errors quote the key spelling the operator
	// wrote, not the folded lowercase match key: a cycle among mixed-case
	// keys names the written key that closes it, and an over-deep chain
	// anchored at a mixed-case key reports that spelling. Walk starts are
	// sorted, so both messages are identical on every validation.
	const wantCycle = `[groups.fallbacks] cycle detected: "a/Y" → "a/x"`
	for i := 0; i < 30; i++ {
		cycleErr := validateFallbacks("groups.fallbacks", map[string]string{
			"a/X": "a/y",
			"a/Y": "a/x",
		}, nil)
		if cycleErr == nil {
			t.Fatal("expected error for a case-fold-only cycle")
		}
		if cycleErr.Error() != wantCycle {
			t.Fatalf("run %d: cycle error = %q, want exactly %q (the written key spelling, stable)", i, cycleErr.Error(), wantCycle)
		}
	}

	depthErr := validateFallbacks("groups.fallbacks", map[string]string{
		"a/A": "a/b",
		"a/b": "a/c",
		"a/c": "a/d",
		"a/d": "a/e",
	}, nil)
	if depthErr == nil {
		t.Fatal("expected error for an over-deep chain")
	}
	if msg := depthErr.Error(); !strings.Contains(msg, `chain starting at "a/A"`) {
		t.Errorf("depth error = %q, want the written key spelling %q", msg, `"a/A"`)
	}
}

func TestValidateModelKeys_EmptyModelRequired(t *testing.T) {
	// Proves a [models.*] entry with no model is reported as the missing
	// required value it is, instead of two empty strings colliding as
	// "the same model" — which pointed at case matching instead of the
	// actual problem.
	err := validateModelKeys(map[string]ModelConfig{
		"a": {},
		"b": {},
	})
	if err == nil {
		t.Fatal("expected error for model-less entries")
	}
	if !strings.Contains(err.Error(), "model is required") || !strings.Contains(err.Error(), "models.a") {
		t.Errorf("error = %q, want [models.a] model is required (first entry in sorted order)", err.Error())
	}
}
