package config

import (
	"reflect"
	"sort"
	"testing"
)

// TestResolveAllowedAccess_CascadeUnchanged pins the allowed_users /
// allowed_users_only cascade through the shared resolver — the tables the
// telegram and discord private resolvers were pinned by before their bodies
// were lifted into ResolveAllowedAccess (#2459 A1), with the same expected
// values, plus the platform scoping slack relies on (a foreign platform's
// entries are invisible).
func TestResolveAllowedAccess_CascadeUnchanged(t *testing.T) {
	mkAgent := func(users []string, flag *bool) AgentConfig {
		return AgentConfig{Platforms: []PlatformConfig{{
			ID: "telegram", Access: AccessConfig{AllowedUsers: users, AllowedUsersOnly: flag},
		}}}
	}
	mkGlobal := func(users []string, flag *bool) *Config {
		return &Config{Platforms: []PlatformConfig{{
			ID: "telegram", Access: AccessConfig{AllowedUsers: users, AllowedUsersOnly: flag},
		}}}
	}
	boolPtr := func(v bool) *bool { return &v }

	// User merge (the old telegram/discord user-merge tables; SuperveneSlice
	// order is global-first-then-agent-only, so compare as sorted sets).
	sorted := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return out
	}
	if got := ResolveAllowedAccess(mkGlobal([]string{"222", "333"}, nil), mkAgent([]string{"111", "222"}, nil), "telegram").Users; !reflect.DeepEqual(sorted(got), []string{"111", "222", "333"}) {
		t.Errorf("merged users = %v, want {111 222 333}", got)
	}
	if got := ResolveAllowedAccess(mkGlobal([]string{"333"}, nil), AgentConfig{}, "telegram").Users; !reflect.DeepEqual(sorted(got), []string{"333"}) {
		t.Errorf("global only = %v, want {333}", got)
	}
	if got := ResolveAllowedAccess(&Config{}, mkAgent([]string{"111"}, nil), "telegram").Users; !reflect.DeepEqual(sorted(got), []string{"111"}) {
		t.Errorf("agent only = %v, want {111}", got)
	}
	if got := ResolveAllowedAccess(&Config{}, AgentConfig{}, "telegram").Users; len(got) != 0 {
		t.Errorf("no entries = %v, want empty", got)
	}
	// Discord's old table: merge with dedup.
	dCfg := &Config{Platforms: []PlatformConfig{{ID: "discord", Access: AccessConfig{AllowedUsers: []string{"2", "3"}}}}}
	dAcfg := AgentConfig{Platforms: []PlatformConfig{{ID: "discord", Access: AccessConfig{AllowedUsers: []string{"1", "2"}}}}}
	if got := ResolveAllowedAccess(dCfg, dAcfg, "discord").Users; !reflect.DeepEqual(sorted(got), []string{"1", "2", "3"}) {
		t.Errorf("discord merged = %v, want {1 2 3}", got)
	}

	// Flag cascade (the old telegram/discord flag tables).
	for _, tc := range []struct {
		name          string
		agent, global *bool
		want          bool
	}{
		{"agent true global false", boolPtr(true), boolPtr(false), true},
		{"agent false global true", boolPtr(false), boolPtr(true), false},
		{"agent nil global true", nil, boolPtr(true), true},
		{"agent nil global false", nil, boolPtr(false), false},
		{"agent nil global nil", nil, nil, true},
	} {
		if got := ResolveAllowedAccess(mkGlobal(nil, tc.global), mkAgent(nil, tc.agent), "telegram").Only; got != tc.want {
			t.Errorf("%s: Only = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := ResolveAllowedAccess(&Config{}, mkAgent(nil, boolPtr(false)), "telegram").Only; got != false {
		t.Errorf("no global entry: Only = %v, want false", got)
	}
	if got := ResolveAllowedAccess(mkGlobal(nil, boolPtr(false)), AgentConfig{}, "telegram").Only; got != false {
		t.Errorf("no agent entry: Only = %v, want false", got)
	}
	if got := ResolveAllowedAccess(&Config{}, AgentConfig{}, "telegram").Only; got != true {
		t.Errorf("no entries: Only = %v, want true (strict default)", got)
	}

	// Platform scoping: entries on another platform never leak in.
	foreign := &Config{Platforms: []PlatformConfig{{ID: "slack", Access: AccessConfig{AllowedUsers: []string{"U1"}, AllowedUsersOnly: boolPtr(false)}}}}
	got := ResolveAllowedAccess(foreign, AgentConfig{}, "slack")
	if !reflect.DeepEqual(got.Users, []string{"U1"}) || got.Only != false {
		t.Errorf("slack-scoped resolution = %+v, want users [U1] only=false", got)
	}
	if got := ResolveAllowedAccess(&Config{}, AgentConfig{}, "slack"); got.Only != true || len(got.Users) != 0 {
		t.Errorf("slack with no entries = %+v, want strict empty", got)
	}
}
