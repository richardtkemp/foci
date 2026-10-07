package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseQuietWindow(t *testing.T) {
	// Proves the one parser of the compaction_quiet_hours format: valid
	// plain and midnight-wrapping windows parse, the empty string is the
	// off value, and malformed values or start == end are errors.
	cases := []struct {
		in      string
		want    QuietWindow
		wantErr bool
	}{
		{in: "", want: QuietWindow{}},
		{in: "01:00-05:00", want: QuietWindow{StartMin: 60, EndMin: 300}},
		{in: "23:00-07:00", want: QuietWindow{StartMin: 23 * 60, EndMin: 7 * 60}},
		{in: "00:00-23:59", want: QuietWindow{StartMin: 0, EndMin: 23*60 + 59}},
		{in: "22:00-22:00", wantErr: true}, // start == end
		{in: "23:00", wantErr: true},       // no dash
		{in: "9-17", wantErr: true},        // no HH:MM
		{in: "25:00-07:00", wantErr: true}, // hour out of range
		{in: "23:00-07:99", wantErr: true}, // minute out of range
		{in: "23:00-:07", wantErr: true},   // non-numeric
		{in: "a:00-b:00", wantErr: true},
		{in: "01:00-05:00-09:00", wantErr: true}, // extra segment
	}
	for _, tc := range cases {
		got, err := ParseQuietWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseQuietWindow(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseQuietWindow(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseQuietWindow(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestQuietWindowContains(t *testing.T) {
	// Proves window membership is a pure function of (window, time) with
	// start inclusive and end exclusive, including across the midnight wrap
	// of "23:00-07:00", and that the zero window (feature off) contains
	// nothing. Times in UTC so the host timezone cannot shift the verdict.
	at := func(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, time.UTC) }
	wrap, _ := ParseQuietWindow("23:00-07:00")
	plain, _ := ParseQuietWindow("09:00-17:00")

	cases := []struct {
		w    QuietWindow
		t    time.Time
		want bool
	}{
		{wrap, at(2, 0), true},           // after midnight, inside the wrap
		{wrap, at(23, 0), true},          // start is inclusive
		{wrap, at(23, 1), true},          // late night, inside
		{wrap, at(7, 0), false},          // end is exclusive
		{wrap, at(6, 59), true},          // one minute before the end
		{wrap, at(12, 0), false},         // midday, outside the wrap
		{wrap, at(22, 59), false},        // one minute before the start
		{plain, at(9, 0), true},          // start inclusive
		{plain, at(16, 59), true},        // inside
		{plain, at(17, 0), false},        // end exclusive
		{plain, at(8, 59), false},        // before the start
		{QuietWindow{}, at(2, 0), false}, // off: the zero window contains nothing
	}
	for _, tc := range cases {
		if got := tc.w.Contains(tc.t); got != tc.want {
			t.Errorf("QuietWindow{%d,%d}.Contains(%02d:%02d) = %v, want %v",
				tc.w.StartMin, tc.w.EndMin, tc.t.Hour(), tc.t.Minute(), got, tc.want)
		}
	}
}

func TestQuietWindowOccurrenceStart(t *testing.T) {
	// Proves the once-per-window anchor: the occurrence covering a time
	// began at today's start minute, or yesterday's when a wrapping window
	// has crossed midnight.
	wrap, _ := ParseQuietWindow("23:00-07:00")
	plain, _ := ParseQuietWindow("09:00-17:00")

	// 02:00 inside 23:00-07:00 → the occurrence began yesterday 23:00.
	got := wrap.OccurrenceStart(time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC))
	want := time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("wrap.OccurrenceStart(02:00) = %v, want %v", got, want)
	}
	// 23:30 inside the same window → today 23:00.
	got = wrap.OccurrenceStart(time.Date(2026, 10, 7, 23, 30, 0, 0, time.UTC))
	want = time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("wrap.OccurrenceStart(23:30) = %v, want %v", got, want)
	}
	// 11:00 inside 09:00-17:00 → today 09:00.
	got = plain.OccurrenceStart(time.Date(2026, 10, 7, 11, 0, 0, 0, time.UTC))
	want = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("plain.OccurrenceStart(11:00) = %v, want %v", got, want)
	}
}

// loadQuietConfig writes the TOML through the real Load path so validation,
// tag defaults and decoding are all exercised.
func loadQuietConfig(t *testing.T, toml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	return err
}

const quietValidBase = "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n"

func TestValidateQuietCompactionConfig(t *testing.T) {
	// Proves config load fails on a malformed quiet window (global or
	// per-agent), start == end, an out-of-range threshold, or a bad
	// min-idle — with an error that names the offending key — and that a
	// fully valid configuration loads.
	cases := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name:    "global window malformed",
			toml:    "[sessions]\ncompaction_quiet_hours = \"2300-0700\"\n",
			wantErr: "[sessions] compaction_quiet_hours = \"2300-0700\"",
		},
		{
			name:    "global window start equals end",
			toml:    "[sessions]\ncompaction_quiet_hours = \"23:00-23:00\"\n",
			wantErr: "[sessions] compaction_quiet_hours = \"23:00-23:00\"",
		},
		{
			name:    "agent window malformed",
			toml:    quietValidBase + "[sessions]\ncompaction_quiet_hours = \"23:00-07:00\"\n\n[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_quiet_hours = \"nope\"\n",
			wantErr: "agent \"test\" [sessions] compaction_quiet_hours = \"nope\"",
		},
		{
			name:    "global threshold zero",
			toml:    "[sessions]\ncompaction_quiet_threshold = 0.0\n",
			wantErr: "[sessions] compaction_quiet_threshold = 0",
		},
		{
			name:    "global threshold above one",
			toml:    "[sessions]\ncompaction_quiet_threshold = 1.5\n",
			wantErr: "[sessions] compaction_quiet_threshold = 1.5",
		},
		{
			name:    "agent threshold out of range",
			toml:    quietValidBase + "[sessions]\ncompaction_quiet_hours = \"23:00-07:00\"\n\n[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_quiet_threshold = -0.1\n",
			wantErr: "agent \"test\" [sessions] compaction_quiet_threshold = -0.1",
		},
		{
			name:    "global min idle not a duration",
			toml:    "[sessions]\ncompaction_quiet_min_idle = \"tomorrow\"\n",
			wantErr: "[sessions] compaction_quiet_min_idle = \"tomorrow\"",
		},
		{
			name:    "global min idle zero",
			toml:    "[sessions]\ncompaction_quiet_min_idle = \"0s\"\n",
			wantErr: "[sessions] compaction_quiet_min_idle = \"0s\"",
		},
		{
			name:    "agent min idle negative",
			toml:    quietValidBase + "[sessions]\ncompaction_quiet_hours = \"23:00-07:00\"\n\n[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_quiet_min_idle = \"-5m\"\n",
			wantErr: "agent \"test\" [sessions] compaction_quiet_min_idle = \"-5m\"",
		},
		{
			name: "valid wrapping window with all knobs, global and agent",
			toml: quietValidBase + "[sessions]\ncompaction_quiet_hours = \"23:00-07:00\"\ncompaction_quiet_threshold = 0.6\ncompaction_quiet_min_idle = \"45m\"\n\n[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_quiet_hours = \"01:00-05:00\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := loadQuietConfig(t, tc.toml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestResolveQuietCompactionDefaults(t *testing.T) {
	// Proves the resolved defaults with nothing configured: the feature is ON
	// by default (Dick 2026-10-07) with the 23:00-07:00 window, the threshold
	// 0.5, the min idle 30m — and an explicit global "" turns it off.
	got := Resolve(&Config{}, AgentConfig{ID: "a"}).Compaction.Quiet
	want := ResolvedQuietCompaction{Window: "23:00-07:00", Threshold: 0.5, MinIdle: "30m"}
	if got != want {
		t.Errorf("resolved quiet compaction = %+v, want %+v", got, want)
	}
	off := &Config{Sessions: SessionsConfig{CompactionConfig: CompactionConfig{CompactionQuietHours: Ptr("")}}}
	if w := Resolve(off, AgentConfig{ID: "a"}).Compaction.Quiet.Window; w != "" {
		t.Errorf("global compaction_quiet_hours = \"\" resolved to window %q, want \"\" (off)", w)
	}
}

func TestResolveQuietCompactionMergeOverrideAndOptOut(t *testing.T) {
	// Proves the per-agent cascade: an agent inherits a global window,
	// overrides the threshold and min idle, and can opt out of a global
	// window with an explicit empty string (Merge picks the first non-nil
	// pointer, so "" wins over the global value).
	cfg := &Config{Sessions: SessionsConfig{CompactionConfig: CompactionConfig{
		CompactionQuietHours:     Ptr("23:00-07:00"),
		CompactionQuietThreshold: Ptr(0.5),
	}}}
	acfg := AgentConfig{ID: "a", Sessions: AgentSessionsOverride{CompactionConfig: CompactionConfig{
		CompactionQuietThreshold: Ptr(0.75),
		CompactionQuietMinIdle:   Ptr("10m"),
	}}}

	got := Resolve(cfg, acfg).Compaction.Quiet
	want := ResolvedQuietCompaction{Window: "23:00-07:00", Threshold: 0.75, MinIdle: "10m"}
	if got != want {
		t.Fatalf("agent override resolved to %+v, want %+v", got, want)
	}

	// The explicit "" opt-out: no window for this agent even though the
	// global one is set.
	acfg.Sessions.CompactionConfig.CompactionQuietHours = Ptr("")
	if got := Resolve(cfg, acfg).Compaction.Quiet.Window; got != "" {
		t.Errorf("per-agent \"\" opt-out resolved window = %q, want empty", got)
	}
}

func TestQuietCompactionFieldsHotEvent(t *testing.T) {
	// Proves all six registry rows exist and are live-appliable
	// (NeedsRestart=false) — a hot row without an applier is caught by
	// cmd/foci-gw's TestLiveApplyCoversHotFields; this pins the tag side.
	for _, addr := range []string{
		"sessions.compaction_quiet_hours",
		"sessions.compaction_quiet_threshold",
		"sessions.compaction_quiet_min_idle",
		"agent.sessions.compaction_quiet_hours",
		"agent.sessions.compaction_quiet_threshold",
		"agent.sessions.compaction_quiet_min_idle",
	} {
		f, ok := LookupField(addr)
		if !ok {
			t.Errorf("LookupField(%q) returned false", addr)
			continue
		}
		if f.NeedsRestart {
			t.Errorf("LookupField(%q).NeedsRestart = true, want false (hot:\"event\")", addr)
		}
	}
}
