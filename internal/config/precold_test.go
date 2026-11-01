package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadPrecoldConfig loads the TOML through the real Load path (tag defaults,
// decoding, validation).
func loadPrecoldConfig(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

func TestResolvePrecoldCompactionDefaults(t *testing.T) {
	// Off by default (Dick, 2026-10-10), threshold 0.5, lead 10m — both for a
	// struct-literal config and through Load's tag defaults.
	want := ResolvedPrecoldCompaction{Enabled: false, Threshold: 0.5, Lead: "10m"}
	if got := Resolve(&Config{}, AgentConfig{ID: "a"}).Compaction.Precold; got != want {
		t.Errorf("struct-literal default = %+v, want %+v", got, want)
	}
	cfg, err := loadPrecoldConfig(t, quietValidBase+"[[agents]]\nid = \"a\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Resolve(cfg, cfg.Agents[0]).Compaction.Precold; got != want {
		t.Errorf("Load default = %+v, want %+v", got, want)
	}
}

func TestResolvePrecoldCompactionGlobalAndAgent(t *testing.T) {
	cfg := &Config{Sessions: SessionsConfig{CompactionConfig: CompactionConfig{
		CompactionPrecold:          Ptr(true),
		CompactionPrecoldThreshold: Ptr(0.7),
	}}}
	acfg := AgentConfig{ID: "a", Sessions: AgentSessionsOverride{CompactionConfig: CompactionConfig{
		CompactionPrecoldLead: Ptr("5m"),
	}}}
	want := ResolvedPrecoldCompaction{Enabled: true, Threshold: 0.7, Lead: "5m"}
	if got := Resolve(cfg, acfg).Compaction.Precold; got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
	// An agent can turn a global "on" off.
	acfg.Sessions.CompactionConfig.CompactionPrecold = Ptr(false)
	if Resolve(cfg, acfg).Compaction.Precold.Enabled {
		t.Error("per-agent compaction_precold = false did not override the global true")
	}
}

func TestValidatePrecoldCompactionConfig(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{"threshold zero", "[sessions]\ncompaction_precold_threshold = 0.0\n", "[sessions] compaction_precold_threshold = 0"},
		{"threshold above one", "[sessions]\ncompaction_precold_threshold = 1.2\n", "[sessions] compaction_precold_threshold = 1.2"},
		{"lead not a duration", "[sessions]\ncompaction_precold_lead = \"soon\"\n", "[sessions] compaction_precold_lead = \"soon\""},
		{"lead zero", "[sessions]\ncompaction_precold_lead = \"0s\"\n", "[sessions] compaction_precold_lead = \"0s\""},
		{"agent lead negative", quietValidBase + "[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_precold_lead = \"-1m\"\n", "agent \"test\" [sessions] compaction_precold_lead = \"-1m\""},
		{"valid, global and agent", quietValidBase + "[sessions]\ncompaction_precold = true\ncompaction_precold_threshold = 0.6\ncompaction_precold_lead = \"15m\"\n\n[[agents]]\nid = \"test\"\n[agents.sessions]\ncompaction_precold = false\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadPrecoldConfig(t, tc.toml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestPrecoldNeverFiresWarnings(t *testing.T) {
	// Requirement 6: warn when pre-cold is enabled while keepalive is
	// enabled with max_user_idle = 0, and only then.
	cases := []struct {
		name  string
		toml  string
		wants []string // agent ids warned about
	}{
		{"enabled, keepalive on, max_user_idle 0", "[keepalive]\nenabled = true\nmax_user_idle = \"0\"\n[sessions]\ncompaction_precold = true\n", []string{"a", "b"}},
		{"enabled, keepalive on, max_user_idle 0s", "[keepalive]\nenabled = true\nmax_user_idle = \"0s\"\n[sessions]\ncompaction_precold = true\n", []string{"a", "b"}},
		{"enabled, keepalive on, default max_user_idle", "[keepalive]\nenabled = true\n[sessions]\ncompaction_precold = true\n", nil},
		{"enabled, keepalive off, max_user_idle 0", "[keepalive]\nenabled = false\nmax_user_idle = \"0\"\n[sessions]\ncompaction_precold = true\n", nil},
		{"disabled, keepalive on, max_user_idle 0", "[keepalive]\nenabled = true\nmax_user_idle = \"0\"\n", nil},
		{"per-agent enable only", "[keepalive]\nenabled = true\nmax_user_idle = \"0\"\n", []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := quietValidBase + tc.toml + "\n[[agents]]\nid = \"a\"\n\n[[agents]]\nid = \"b\"\n"
			if tc.name == "per-agent enable only" {
				toml += "[agents.sessions]\ncompaction_precold = true\n"
			}
			cfg, err := loadPrecoldConfig(t, toml)
			if err != nil {
				t.Fatal(err)
			}
			got := PrecoldNeverFiresWarnings(cfg)
			if len(got) != len(tc.wants) {
				t.Fatalf("warnings = %q, want one per agent %v", got, tc.wants)
			}
			for i, id := range tc.wants {
				if !strings.Contains(got[i], "agent \""+id+"\"") || !strings.Contains(got[i], "max_user_idle = 0") {
					t.Errorf("warning %q does not name agent %q and the cause", got[i], id)
				}
			}
		})
	}
}

func TestKeepaliveMaxUserIdleUnlimited(t *testing.T) {
	for in, want := range map[string]bool{"": true, "0": true, "0s": true, "-1h": true, "bogus": true, "1h": false, "96h": false} {
		if got := KeepaliveMaxUserIdleUnlimited(in); got != want {
			t.Errorf("KeepaliveMaxUserIdleUnlimited(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPrecoldCompactionFieldsHotEvent(t *testing.T) {
	for _, addr := range []string{
		"sessions.compaction_precold",
		"sessions.compaction_precold_threshold",
		"sessions.compaction_precold_lead",
		"agent.sessions.compaction_precold",
		"agent.sessions.compaction_precold_threshold",
		"agent.sessions.compaction_precold_lead",
	} {
		f, ok := LookupField(addr)
		if !ok {
			t.Errorf("LookupField(%q) returned false", addr)
			continue
		}
		if f.NeedsRestart {
			t.Errorf("LookupField(%q).NeedsRestart = true, want false", addr)
		}
	}
}
