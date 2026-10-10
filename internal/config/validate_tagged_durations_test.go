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

// TestLoadRejectsPerAgentTelegramLongPollTimeout proves a bad
// long_poll_timeout inside an [[agents.platforms]] block is refused at Load
// with the walk's per-agent naming. Nothing checked it before the field was
// tagged type:"duration" — the hand-written table in Validate loops global
// [[platforms]] only, so the bot silently kept the default at run time.
func TestLoadRejectsPerAgentTelegramLongPollTimeout(t *testing.T) {
	toml := durationTestBase + "\n[[agents.platforms]]\nid = \"telegram\"\nfacet_session_ttl = \"30m\"\n\n[agents.platforms.telegram]\nlong_poll_timeout = \"banana\""
	err := loadDurationFixture(t, toml)
	if err == nil {
		t.Fatal("expected Load to reject the per-agent long_poll_timeout, got nil — the bot silently keeps the default at run time")
	}
	want := `invalid config: agent "test" [platforms.telegram.telegram] long_poll_timeout = "banana":`
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want prefix %q", err.Error(), want)
	}
}

// TestLoadRejectsInvalidTaggedSchedules proves a type:"schedule" value that is
// neither a "HH:MM" clock time nor a positive duration is refused at Load —
// globally and per agent — instead of loading and then silently disabling
// consolidation or the daily reset with a run-time warning on every tick.
func TestLoadRejectsInvalidTaggedSchedules(t *testing.T) {
	tests := []struct {
		name       string
		toml       string
		wantPrefix string
	}{
		{
			"global consolidation_time",
			durationTestBase + "\n[maintenance]\nconsolidation_time = \"banana\"",
			`[maintenance] consolidation_time = "banana":`,
		},
		{
			"global reset_time 25:00",
			durationTestBase + "\n[maintenance]\nreset_time = \"25:00\"",
			`[maintenance] reset_time = "25:00":`,
		},
		{
			"global reset_time 0 (non-positive duration)",
			durationTestBase + "\n[maintenance]\nreset_time = \"0\"",
			`[maintenance] reset_time = "0":`,
		},
		{
			"agent consolidation_time -5m",
			durationTestBase + "\n[agents.maintenance]\nconsolidation_time = \"-5m\"",
			`agent "test" [maintenance] consolidation_time = "-5m":`,
		},
		{
			"agent reset_time",
			durationTestBase + "\n[agents.maintenance]\nreset_time = \"banana\"",
			`agent "test" [maintenance] reset_time = "banana":`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadDurationFixture(t, tt.toml)
			if err == nil {
				t.Fatal("expected Load to reject the bad schedule, got nil — consolidation or the reset would never run at run time")
			}
			if want := "invalid config: " + tt.wantPrefix + " want HH:MM or a positive duration like 20h"; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("error = %q, want prefix %q", err.Error(), want)
			}
		})
	}
}

// TestLoadTaggedScheduleFieldRegistryCoverage walks every FieldSchedule row of
// the field registry — the same rows /config set and the app editor offer — in
// section "agent" and in every global section except the skipped ones (same
// exclusions as the duration coverage walk). Writing "banana" through the
// row's TOML place must make Load fail naming the key, so a schedule row can
// never silently drop out of load-time checking.
func TestLoadTaggedScheduleFieldRegistryCoverage(t *testing.T) {
	skipped := map[string]bool{
		"platforms": true, // [[platforms]] is a list, not a [section]
		"tracing":   true, // validated only when enabled
		"bitwarden": true, // validated only when enabled
	}
	found := 0
	for _, section := range FieldSections() {
		if skipped[section] {
			continue
		}
		for _, f := range FieldsInSection(section) {
			if f.Type != FieldSchedule {
				continue
			}
			found++
			t.Run(section+"."+f.Key, func(t *testing.T) {
				// Fresh TOML per row — subtests never share fixture state.
				toml := durationTestBase + "\n"
				if section == "agent" {
					tablePath, leaf, dotted := cutLastDot(f.Key)
					if !dotted {
						t.Fatalf("agent-section schedule row %q is flat — no [agents.<table>] place to write it; extend the test", f.Key)
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
	if found == 0 {
		t.Fatal("no FieldSchedule rows found in the registry — the schedule tag is missing from every field")
	}
}

// TestLoadValidTaggedSchedules is a characterisation test: schedule shapes
// that load today must keep loading once the fields are tagged — the clock
// forms Dick's live config uses (03:45, 4:20, 00:00, 23:59), positive
// durations, and the explicit empty string (reset_time = "" means "never") —
// in both [maintenance] and [agents.maintenance], plus a well-formed
// long_poll_timeout in the per-agent and global telegram blocks.
func TestLoadValidTaggedSchedules(t *testing.T) {
	for _, v := range []string{"03:45", "4:20", "00:00", "23:59", "20h", "90m", ""} {
		t.Run("global maintenance schedule = "+v, func(t *testing.T) {
			toml := durationTestBase + fmt.Sprintf("\n[maintenance]\nconsolidation_time = %q\nreset_time = %q", v, v)
			if err := loadDurationFixture(t, toml); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
		t.Run("agent maintenance schedule = "+v, func(t *testing.T) {
			toml := durationTestBase + fmt.Sprintf("\n[agents.maintenance]\nconsolidation_time = %q\nreset_time = %q", v, v)
			if err := loadDurationFixture(t, toml); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	t.Run("agent telegram long_poll_timeout = 30s", func(t *testing.T) {
		toml := durationTestBase + "\n[[agents.platforms]]\nid = \"telegram\"\n\n[agents.platforms.telegram]\nlong_poll_timeout = \"30s\""
		if err := loadDurationFixture(t, toml); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("global telegram long_poll_timeout = 30s", func(t *testing.T) {
		toml := durationTestBase + "\n[[platforms]]\nid = \"telegram\"\n\n[platforms.telegram]\nlong_poll_timeout = \"30s\""
		if err := loadDurationFixture(t, toml); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
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
