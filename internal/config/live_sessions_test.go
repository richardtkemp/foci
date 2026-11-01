package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const liveSessionsBase = "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[[agents]]\nid = \"test\"\n"

func loadLiveSessionsTOML(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

// TestLiveSessionsDefaults proves the #2280 caps default to 0 (no cap) at both
// levels and the global idle_timeout to empty (3h), so an unset config keeps
// today's behaviour.
func TestLiveSessionsDefaults(t *testing.T) {
	cfg, err := loadLiveSessionsTOML(t, liveSessionsBase)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sessions.MaxLiveSessions != 0 {
		t.Errorf("[sessions] max_live_sessions = %d, want 0", cfg.Sessions.MaxLiveSessions)
	}
	if cfg.Agents[0].Sessions.MaxLiveSessions != nil {
		t.Errorf("agent max_live_sessions = %d, want unset", *cfg.Agents[0].Sessions.MaxLiveSessions)
	}
	if cfg.Sessions.IdleTimeout != "" {
		t.Errorf("[sessions] idle_timeout = %q, want empty", cfg.Sessions.IdleTimeout)
	}
}

// TestValidateLiveSessions proves a negative cap at either level, and an
// idle_timeout at either level that does not parse or is not positive, fail
// load with an error naming the key (and the agent at agent level).
func TestValidateLiveSessions(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want []string
	}{
		{"global negative cap", liveSessionsBase + "[sessions]\nmax_live_sessions = -1\n", []string{"[sessions] max_live_sessions", "must not be negative"}},
		{"agent negative cap", liveSessionsBase + "[agents.sessions]\nmax_live_sessions = -2\n", []string{`agent "test"`, "max_live_sessions", "must not be negative"}},
		{"global bad idle", liveSessionsBase + "[sessions]\nidle_timeout = \"soon\"\n", []string{"[sessions] idle_timeout"}},
		{"global zero idle", liveSessionsBase + "[sessions]\nidle_timeout = \"0s\"\n", []string{"[sessions] idle_timeout", "positive"}},
		{"agent bad idle", liveSessionsBase + "backend = \"claude-code\"\n[agents.backend_config]\nidle_timeout = \"3 hours\"\n", []string{`agent "test"`, "idle_timeout"}},
		{"agent negative idle", liveSessionsBase + "backend = \"claude-code\"\n[agents.backend_config]\nidle_timeout = \"-5m\"\n", []string{`agent "test"`, "idle_timeout", "positive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadLiveSessionsTOML(t, tt.toml)
			if err == nil {
				t.Fatal("expected a load error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
	ok := liveSessionsBase + "backend = \"claude-code\"\n[agents.backend_config]\nidle_timeout = \"10m\"\n[agents.sessions]\nmax_live_sessions = 0\n[sessions]\nmax_live_sessions = 4\nidle_timeout = \"1h\"\n"
	if _, err := loadLiveSessionsTOML(t, ok); err != nil {
		t.Fatalf("valid config failed to load: %v", err)
	}
}

// TestMaxLiveSessionsAgentDoesNotInherit proves the per-agent cap is not a
// child of the gateway total: the registry marks it NoInherit, an unset agent
// value resolves to the built-in default (no cap) even with [sessions] set,
// and the running value is the agent's own.
func TestMaxLiveSessionsAgentDoesNotInherit(t *testing.T) {
	f, ok := LookupField("agent.sessions.max_live_sessions")
	if !ok {
		t.Fatal("agent.sessions.max_live_sessions not in the registry")
	}
	if !f.NoInherit {
		t.Fatal("agent.sessions.max_live_sessions should be NoInherit")
	}
	global := map[string]string{"sessions.max_live_sessions": "10"}
	v, src := ResolveAgentValue(global, nil, "test", f, nil)
	if v != f.Default || !strings.Contains(src, "default") {
		t.Fatalf("unset agent value = %q (%s), want the built-in default, not the gateway total", v, src)
	}
	cfg := &Config{Sessions: SessionsConfig{MaxLiveSessions: 10}}
	if got := AgentRunningValue(cfg, AgentConfig{}, "sessions.max_live_sessions"); got == "10" {
		t.Fatal("running agent value inherited the gateway total")
	}
	// Characterisation: a sibling override key still inherits.
	g, _ := LookupField("agent.sessions.ephemeral_retention_days")
	if g.NoInherit {
		t.Fatal("ephemeral_retention_days must keep inheriting")
	}
}

// TestLiveSessionsRegistryRows proves the three #2280 keys are registry rows
// that need a restart (no hot tag) and that the per-agent cap has an agent
// display row when set.
func TestLiveSessionsRegistryRows(t *testing.T) {
	for _, k := range []string{"sessions.max_live_sessions", "agent.sessions.max_live_sessions", "sessions.idle_timeout"} {
		f, ok := LookupField(k)
		if !ok {
			t.Errorf("%s not in the registry", k)
			continue
		}
		if !f.NeedsRestart {
			t.Errorf("%s should need a restart", k)
		}
	}
	var found string
	for _, r := range collectAgentRows(AgentConfig{Sessions: AgentSessionsOverride{MaxLiveSessions: Ptr(3)}}) {
		if r.Key == "max_live_sessions" {
			found = r.Value
		}
	}
	if found != "3" {
		t.Errorf("agent max_live_sessions display row = %q, want 3", found)
	}
}
