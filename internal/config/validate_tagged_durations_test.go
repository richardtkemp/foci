package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// durationTestBase is the minimal fixture every duration-load test builds on:
// a valid powerful group plus one agent block, the same shape the existing
// Load tests use.
const durationTestBase = "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n"

// loadDurationFixture writes toml to a fresh temp foci.toml and Loads it,
// returning Load's error (nil on success).
func loadDurationFixture(t *testing.T, toml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	return err
}

// TestLoadRejectsInvalidTaggedDurations proves a hand-edited duration that
// time.ParseDuration rejects is refused at Load — in global sections, per-agent
// blocks, per-agent and global [[platforms]] entries, and [models.*] map values —
// with the where/key/value error prefix the per-agent quiet check already uses.
// Map entries are visited in sorted key order, so with two bad models the
// alphabetically-first one is named.
func TestLoadRejectsInvalidTaggedDurations(t *testing.T) {
	tests := []struct {
		name       string
		toml       string
		wantPrefix string
	}{
		{
			"global keepalive interval",
			durationTestBase + "\n[keepalive]\ninterval = \"banana\"",
			`[keepalive] interval = "banana":`,
		},
		{
			"global keepalive max_user_idle",
			durationTestBase + "\n[keepalive]\nmax_user_idle = \"4days\"",
			`[keepalive] max_user_idle = "4days":`,
		},
		{
			"agent keepalive interval",
			durationTestBase + "\n[keepalive]\ninterval = \"55m\"\n\n[agents.keepalive]\ninterval = \"banana\"",
			`agent "test" [keepalive] interval = "banana":`,
		},
		{
			"agent keepalive max_user_idle",
			durationTestBase + "\n[keepalive]\nmax_user_idle = \"96h\"\n\n[agents.keepalive]\nmax_user_idle = \"4days\"",
			`agent "test" [keepalive] max_user_idle = "4days":`,
		},
		{
			"global scheduler tick_interval",
			durationTestBase + "\n[scheduler]\ntick_interval = \"fast\"",
			`[scheduler] tick_interval = "fast":`,
		},
		{
			"agent behavior group_throttle",
			durationTestBase + "\n[agents.behavior]\ngroup_throttle = \"soon\"",
			`agent "test" [behavior] group_throttle = "soon":`,
		},
		{
			"agent tools tmux_watch_threshold (embedded struct adds no table segment)",
			durationTestBase + "\n[agents.tools]\ntmux_watch_threshold = \"later\"",
			`agent "test" [tools] tmux_watch_threshold = "later":`,
		},
		{
			"agent platform facet_session_ttl",
			durationTestBase + "\n[[agents.platforms]]\nid = \"telegram\"\nfacet_session_ttl = \"soon\"",
			`agent "test" [platforms.telegram] facet_session_ttl = "soon":`,
		},
		{
			"global platform display stream_interval",
			durationTestBase + "\n[[platforms]]\nid = \"telegram\"\n\n[platforms.display]\nstream_interval = \"soon\"",
			`[platforms.telegram.display] stream_interval = "soon":`,
		},
		{
			"model cache_ttl",
			durationTestBase + "\n[models.foo]\nmodel = \"anthropic/claude-haiku-4-5-20251001\"\ncache_ttl = \"banana\"",
			`[models.foo] cache_ttl = "banana":`,
		},
		{
			"model map keys visited in sorted order",
			durationTestBase + "\n[models.zeta]\nmodel = \"anthropic/claude-haiku-4-5-20251001\"\ncache_ttl = \"banana\"\n\n[models.alpha]\nmodel = \"anthropic/claude-haiku-4-5-20251001\"\ncache_ttl = \"banana\"",
			`[models.alpha] cache_ttl = "banana":`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadDurationFixture(t, tt.toml)
			if err == nil {
				t.Fatal("expected Load to reject the bad duration, got nil — it would be logged and ignored at run time")
			}
			if want := "invalid config: " + tt.wantPrefix; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("error = %q, want prefix %q", err.Error(), want)
			}
		})
	}
}

// TestLoadTaggedDurationFieldRegistryCoverage walks every FieldDuration row of
// the field registry — the same rows /config set and the app editor offer — in
// section "agent" and in every global section except "platforms" (a list,
// covered by the direct rows above), "tracing" and "bitwarden" (checked only
// when enabled). Writing "banana" through the row's TOML place must make Load
// fail naming the key, so a row can never silently drop out of load-time
// duration checking again.
func TestLoadTaggedDurationFieldRegistryCoverage(t *testing.T) {
	skipped := map[string]bool{
		"platforms": true, // [[platforms]] is a list, not a [section]
		"tracing":   true, // validated only when enabled
		"bitwarden": true, // validated only when enabled
	}
	for _, section := range FieldSections() {
		if skipped[section] {
			continue
		}
		for _, f := range FieldsInSection(section) {
			if f.Type != FieldDuration {
				continue
			}
			t.Run(section+"."+f.Key, func(t *testing.T) {
				// Fresh TOML per row — subtests never share fixture state.
				toml := durationTestBase + "\n"
				if section == "agent" {
					tablePath, leaf, dotted := cutLastDot(f.Key)
					if !dotted {
						t.Fatalf("agent-section duration row %q is flat — no [agents.<table>] place to write it; extend the test", f.Key)
					}
					toml += fmt.Sprintf("[agents.%s]\n%s = \"banana\"", tablePath, leaf)
				} else {
					toml += fmt.Sprintf("[%s]\n%s = \"banana\"", section, f.Key)
				}

				err := loadDurationFixture(t, toml)
				if err == nil {
					t.Fatalf("Load accepted %s.%s = \"banana\" — the row is not checked at load time", section, f.Key)
				}
				_, leaf, _ := cutLastDot(f.Key)
				if want := leaf + ` = "banana"`; !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			})
		}
	}
}

// TestLoadValidTaggedDurations is a characterisation test: values that load
// today must keep loading — valid durations ("10m", "1h30m", "0"), the explicit
// empty string ("empty = default" / "empty disables it" fields), a valid model
// cache_ttl, and disabled [tracing]/[bitwarden] sections carrying unparseable
// durations (their documented never-validate-when-disabled rule).
func TestLoadValidTaggedDurations(t *testing.T) {
	for _, v := range []string{"10m", "1h30m", "0", ""} {
		t.Run("global keepalive interval = "+v, func(t *testing.T) {
			toml := durationTestBase + fmt.Sprintf("\n[keepalive]\ninterval = %q\nmax_user_idle = %q", v, v)
			if err := loadDurationFixture(t, toml); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
		t.Run("agent keepalive interval = "+v, func(t *testing.T) {
			toml := durationTestBase + fmt.Sprintf("\n[agents.keepalive]\ninterval = %q\nmax_user_idle = %q", v, v)
			if err := loadDurationFixture(t, toml); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	t.Run("model cache_ttl = 1h", func(t *testing.T) {
		toml := durationTestBase + "\n[models.foo]\nmodel = \"anthropic/claude-haiku-4-5-20251001\"\ncache_ttl = \"1h\""
		if err := loadDurationFixture(t, toml); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("disabled tracing flush_timeout", func(t *testing.T) {
		toml := durationTestBase + "\n[tracing]\nenabled = false\nflush_timeout = \"banana\""
		if err := loadDurationFixture(t, toml); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("disabled bitwarden secret_ttl", func(t *testing.T) {
		toml := durationTestBase + "\n[bitwarden]\nenabled = false\nsecret_ttl = \"banana\""
		if err := loadDurationFixture(t, toml); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
