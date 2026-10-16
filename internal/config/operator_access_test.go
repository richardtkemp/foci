package config

import (
	"sort"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func platformWithAccess(id string, access AccessConfig) PlatformConfig {
	return PlatformConfig{ID: id, Access: access}
}

func agentWithAccess(id string, access AccessConfig) AgentConfig {
	return AgentConfig{ID: id, Platforms: []PlatformConfig{platformWithAccess("telegram", access), platformWithAccess("discord", AccessConfig{})}}
}

func globalWithAccess(access AccessConfig) *Config {
	return &Config{Platforms: []PlatformConfig{platformWithAccess("telegram", access), platformWithAccess("discord", AccessConfig{})}}
}

func TestOperatorFieldsInRegistry(t *testing.T) {
	// Proves the two operator settings are settable config fields on the
	// platform access section, restart-only, and that their descriptions tell
	// the operator the empty-operators rule (operator_users) and the default
	// command list (non_operator_commands) — the prose cannot drift from
	// DefaultNonOperatorCommands.
	users, ok := LookupField("platforms.access.operator_users")
	if !ok {
		t.Fatal("platforms.access.operator_users missing from the field registry")
	}
	commands, ok := LookupField("platforms.access.non_operator_commands")
	if !ok {
		t.Fatal("platforms.access.non_operator_commands missing from the field registry")
	}
	for _, f := range []ConfigField{users, commands} {
		if f.Type != FieldStringList {
			t.Errorf("%s type = %v, want FieldStringList", f.Key, f.Type)
		}
		if !f.NeedsRestart {
			t.Errorf("%s should be NeedsRestart (like allowed_users)", f.Key)
		}
	}
	if !strings.Contains(users.Description, "no restriction") {
		t.Errorf("operator_users description %q must say empty operators means no restriction", users.Description)
	}
	for _, want := range DefaultNonOperatorCommands() {
		if !strings.Contains(commands.Description, want) {
			t.Errorf("non_operator_commands description %q must name the default command %q", commands.Description, want)
		}
	}
}

func TestDefaultNonOperatorCommands(t *testing.T) {
	// Proves the default non-operator list is exactly help, reset, stop, and
	// that each call returns a fresh copy a caller may mutate.
	got := DefaultNonOperatorCommands()
	if len(got) != 3 || got[0] != "help" || got[1] != "reset" || got[2] != "stop" {
		t.Fatalf("DefaultNonOperatorCommands() = %v, want [help reset stop]", got)
	}
	got[0] = "mutated"
	if again := DefaultNonOperatorCommands(); again[0] != "help" {
		t.Errorf("mutating the result changed the next call: %v", again)
	}
}

func TestResolveOperatorAccess(t *testing.T) {
	// Proves the two-level cascade: global operators apply to every agent of
	// the platform, agent and global operators add up de-duplicated, the
	// agent's command list replaces the global one (an explicitly empty agent
	// list never falls back), and neither set leaks to the other platform.
	strs := func(vals ...string) []string { return vals }

	cases := []struct {
		name         string
		agent        AccessConfig
		global       AccessConfig
		wantUsers    []string
		wantCommands []string
	}{
		{
			name:         "global operators apply to an agent with no platform entry",
			agent:        AccessConfig{},
			global:       AccessConfig{OperatorUsers: strs("111")},
			wantUsers:    strs("111"),
			wantCommands: DefaultNonOperatorCommands(),
		},
		{
			name:         "agent and global operators add up deduplicated",
			agent:        AccessConfig{OperatorUsers: strs("111", "222")},
			global:       AccessConfig{OperatorUsers: strs("222", "333")},
			wantUsers:    strs("111", "222", "333"),
			wantCommands: DefaultNonOperatorCommands(),
		},
		{
			name:         "agent commands replace global commands",
			agent:        AccessConfig{OperatorUsers: strs("111"), NonOperatorCommands: strs("status")},
			global:       AccessConfig{NonOperatorCommands: strs("model", "tmux")},
			wantUsers:    strs("111"),
			wantCommands: strs("status"),
		},
		{
			name:         "global commands used when agent unset",
			agent:        AccessConfig{OperatorUsers: strs("111")},
			global:       AccessConfig{NonOperatorCommands: strs("model")},
			wantUsers:    strs("111"),
			wantCommands: strs("model"),
		},
		{
			name:         "explicitly empty agent list beats a non-empty global list",
			agent:        AccessConfig{OperatorUsers: strs("111"), NonOperatorCommands: []string{}},
			global:       AccessConfig{NonOperatorCommands: strs("model")},
			wantUsers:    strs("111"),
			wantCommands: []string{},
		},
		{
			name:         "explicitly empty global list honoured, not defaulted",
			agent:        AccessConfig{OperatorUsers: strs("111")},
			global:       AccessConfig{NonOperatorCommands: []string{}},
			wantUsers:    strs("111"),
			wantCommands: []string{},
		},
		{
			name:         "neither set means the default list",
			agent:        AccessConfig{OperatorUsers: strs("111")},
			global:       AccessConfig{},
			wantUsers:    strs("111"),
			wantCommands: DefaultNonOperatorCommands(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOperatorAccess(globalWithAccess(tc.global), agentWithAccess("a", tc.agent), "telegram")
			if unordered(got.Users) != unordered(tc.wantUsers) {
				t.Errorf("Users = %v, want %v", got.Users, tc.wantUsers)
			}
			if got.Commands == nil {
				t.Fatalf("Commands = nil, want never nil (an explicit empty list must be observable): %v", got.Commands)
			}
			if unordered(got.Commands) != unordered(tc.wantCommands) {
				t.Errorf("Commands = %v, want %v", got.Commands, tc.wantCommands)
			}

			// The telegram settings must not leak to the discord resolution.
			other := ResolveOperatorAccess(globalWithAccess(tc.global), agentWithAccess("a", tc.agent), "discord")
			if len(other.Users) != 0 {
				t.Errorf("discord Users = %v, want empty (telegram operators do not leak)", other.Users)
			}
		})
	}
}

// unordered compares two ID lists as sets (order carries no meaning).
func unordered(vals []string) string {
	sorted := append([]string(nil), vals...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x00")
}

func TestResolveOperatorAccess_DropsBlankOperatorIDs(t *testing.T) {
	// Proves blank and whitespace-only operator_users entries are dropped
	// before the "are there any operators" decision: [""] must not turn the
	// gate on with nobody who can match, and must never make "" an operator.
	got := ResolveOperatorAccess(globalWithAccess(AccessConfig{OperatorUsers: []string{"111", "", "  "}}),
		agentWithAccess("a", AccessConfig{}), "telegram")
	if len(got.Users) != 1 || got.Users[0] != "111" {
		t.Fatalf("Users = %v, want only the non-blank 111", got.Users)
	}

	allBlank := ResolveOperatorAccess(globalWithAccess(AccessConfig{OperatorUsers: []string{"", "  "}}),
		agentWithAccess("a", AccessConfig{}), "telegram")
	if len(allBlank.Users) != 0 {
		t.Errorf("Users = %v, want empty (blank-only lists restrict nobody)", allBlank.Users)
	}
}

func TestResolveOperatorAccess_TrimsOperatorIDs(t *testing.T) {
	// Proves operator user IDs are resolved trimmed: platform user IDs
	// arrive exact, so a padded entry (" 111 ") would silently restrict the
	// operator it names. Trimming applies at both levels and de-duplicates
	// on the trimmed value.
	got := ResolveOperatorAccess(globalWithAccess(AccessConfig{OperatorUsers: []string{" 222 "}}),
		agentWithAccess("a", AccessConfig{OperatorUsers: []string{" 111 ", "111", "\t333\t"}}), "telegram")
	sort.Strings(got.Users)
	if strings.Join(got.Users, ",") != "111,222,333" {
		t.Fatalf("Users = %v, want the trimmed, de-duplicated [111 222 333]", got.Users)
	}
}

func TestResolveOperatorAccess_DropsRepeatsWithinOneLevel(t *testing.T) {
	// Proves the resolved operator list really is de-duplicated, as its
	// contract says: a repeated ID within one level adds nothing, whether
	// the repeat is at the agent or the global level.
	agentDup := ResolveOperatorAccess(globalWithAccess(AccessConfig{}),
		agentWithAccess("a", AccessConfig{OperatorUsers: []string{"5", "5"}}), "telegram")
	if len(agentDup.Users) != 1 || agentDup.Users[0] != "5" {
		t.Errorf("agent-level repeat: Users = %v, want [5]", agentDup.Users)
	}
	globalDup := ResolveOperatorAccess(globalWithAccess(AccessConfig{OperatorUsers: []string{"7", "7"}}),
		agentWithAccess("a", AccessConfig{}), "telegram")
	if len(globalDup.Users) != 1 || globalDup.Users[0] != "7" {
		t.Errorf("global-level repeat: Users = %v, want [7]", globalDup.Users)
	}
}

func TestResolveOperatorAccess_UnsetPlatformsLeaveNoOperators(t *testing.T) {
	// Characterises the unset case: with no operator settings anywhere the
	// resolved operator list is empty, whatever else the access block holds.
	got := ResolveOperatorAccess(globalWithAccess(AccessConfig{AllowedUsers: []string{"111"}}),
		agentWithAccess("a", AccessConfig{AllowedUsers: []string{"222"}}), "telegram")
	if len(got.Users) != 0 {
		t.Errorf("Users = %v, want empty with no operator settings", got.Users)
	}
}

func TestResolveOperatorAccessFromTOML(t *testing.T) {
	// Proves the operator settings decode from TOML at both levels with the
	// nil-vs-empty distinction intact at the decode step: `non_operator_commands
	// = []` decodes as a non-nil empty slice that ResolveOperatorAccess
	// treats as set, replacing a non-empty global list instead of falling
	// back to it. The defaults cascade that runs between decode and
	// resolution in production is pinned separately, by
	// TestNonOperatorCommandsSurviveDefaultsMerge.
	text := `
[[platforms]]
id = "telegram"
[platforms.access]
operator_users = ["9"]
non_operator_commands = ["help", "reset", "stop", "sessions"]

[[agents]]
id = "scout"

[[agents.platforms]]
id = "telegram"
[agents.platforms.access]
non_operator_commands = []
`
	var cfg Config
	if _, err := tomlParser.Decode(text, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(cfg.Agents))
	}

	got := ResolveOperatorAccess(&cfg, cfg.Agents[0], "telegram")
	if strings.Join(got.Users, ",") != "9" {
		t.Errorf("Users = %v, want the global [9]", got.Users)
	}
	if got.Commands == nil || len(got.Commands) != 0 {
		t.Errorf("Commands = %v, want the agent's explicitly empty list (non-nil, empty)", got.Commands)
	}
}

func TestNonOperatorCommandsSurviveDefaultsMerge(t *testing.T) {
	// Proves the production defaults cascade keeps the operator settings'
	// nil-vs-empty distinction intact: ApplyProviderDefaults (run from
	// main.go before any agent starts) merges each platform's access block
	// with the global one and the provider defaults, and an explicitly empty
	// `non_operator_commands = []` must reach ResolveOperatorAccess as set-but-
	// empty whichever level carries it, while an unset list still inherits a
	// set global one. A merge that judged "set" by length would silently
	// fall both cases back to the wrong list.
	cases := []struct {
		name         string
		text         string
		wantCommands []string
	}{
		{
			name: "agent empty list stays empty over a global non-empty list",
			text: `
[[platforms]]
id = "telegram"
[platforms.access]
operator_users = ["9"]
non_operator_commands = ["help"]

[[agents]]
id = "scout"

[[agents.platforms]]
id = "telegram"
[agents.platforms.access]
operator_users = ["1"]
non_operator_commands = []
`,
			wantCommands: []string{},
		},
		{
			name: "agent unset inherits the global empty list, not the default",
			text: `
[[platforms]]
id = "telegram"
[platforms.access]
operator_users = ["9"]
non_operator_commands = []

[[agents]]
id = "scout"

[[agents.platforms]]
id = "telegram"
[agents.platforms.access]
operator_users = ["1"]
`,
			wantCommands: []string{},
		},
		{
			name: "agent unset inherits a set global list",
			text: `
[[platforms]]
id = "telegram"
[platforms.access]
operator_users = ["9"]
non_operator_commands = ["help"]

[[agents]]
id = "scout"

[[agents.platforms]]
id = "telegram"
[agents.platforms.access]
operator_users = ["1"]
`,
			wantCommands: []string{"help"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if _, err := tomlParser.Decode(tc.text, &cfg); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(cfg.Agents) != 1 {
				t.Fatalf("agents = %d, want 1", len(cfg.Agents))
			}
			// The production defaulter shape: provider defaults carry an
			// access block (RequireMention) with no operator settings, so
			// the operator fields pass through Merge at both levels.
			requireMention := true
			ApplyProviderDefaults(&cfg, func(string) *PlatformConfig {
				return &PlatformConfig{Access: AccessConfig{RequireMention: &requireMention}}
			})

			got := ResolveOperatorAccess(&cfg, cfg.Agents[0], "telegram")
			if len(got.Users) == 0 {
				t.Fatalf("Users = %v, want the operators configured at the levels (gate on)", got.Users)
			}
			if got.Commands == nil {
				t.Fatalf("Commands = nil, want never nil (an explicit empty list must be observable)")
			}
			if strings.Join(got.Commands, ",") != strings.Join(tc.wantCommands, ",") {
				t.Errorf("Commands = %v, want %v", got.Commands, tc.wantCommands)
			}
		})
	}
}

func TestNonOperatorCommandsEmptyListSurvivesConfigSet(t *testing.T) {
	// Proves /config set's file edit produces an empty non_operator_commands
	// list that resolves to "no commands" rather than falling back to the
	// global list or the default.
	base := `
[[platforms]]
id = "telegram"
[platforms.access]
operator_users = ["9"]
non_operator_commands = ["help"]
`
	next, _, err := PlanSetInFile(base, SetTarget{Section: "platforms.access", Key: "non_operator_commands"}, "[]")
	if err != nil {
		t.Fatalf("PlanSetInFile: %v", err)
	}
	var cfg Config
	if _, err := tomlParser.Decode(next, &cfg); err != nil {
		t.Fatalf("decode after set: %v", err)
	}
	got := ResolveOperatorAccess(&cfg, AgentConfig{ID: "a"}, "telegram")
	if len(got.Users) != 1 || got.Users[0] != "9" {
		t.Fatalf("Users = %v, want [9]", got.Users)
	}
	if got.Commands == nil || len(got.Commands) != 0 {
		t.Errorf("Commands = %v, want non-nil empty after setting []", got.Commands)
	}
}

func TestFormatConfigGroupedShowsOperatorSettings(t *testing.T) {
	// Proves the config display shows the operator settings next to
	// allowed_users, at both the global platform row and the agent row, and
	// that an explicitly empty non_operator_commands list is visible while unset
	// settings show nothing.
	cfg := &Config{Platforms: []PlatformConfig{{
		ID: "telegram",
		Access: AccessConfig{
			AllowedUsers:        []string{"111"},
			OperatorUsers:       []string{"999"},
			NonOperatorCommands: []string{},
		},
	}}}
	agent := AgentConfig{ID: "scout", Debug: DebugConfig{MessagesInLog: Ptr(true)}, Platforms: []PlatformConfig{{
		ID: "telegram",
		Access: AccessConfig{
			AllowedUsers:        []string{"222"},
			OperatorUsers:       []string{"888"},
			NonOperatorCommands: []string{"help"},
		},
	}}}

	tables := FormatConfigGrouped(cfg, agent)
	joined := strings.Join(tables, "\n")

	// Global rows: operator settings appear beside allowed_users.
	for _, want := range []string{"operator_users", "999", "non_operator_commands", "allowed_users"} {
		if !strings.Contains(joined, want) {
			t.Errorf("global table missing %q:\n%s", want, joined)
		}
	}

	// Agent rows carry the agent's own operator settings too.
	if !strings.Contains(joined, "888") {
		t.Errorf("agent table missing its operator_users value 888:\n%s", joined)
	}

	// The per-platform loop is the ONE source of the agent table's
	// allowed_users row — the Telegram-only special case it replaced is
	// gone, so the row shows exactly once. messages_in_log is set too so
	// the count is taken with other agent rows interleaved: a Telegram-only
	// leftover grafted onto any earlier branch of collectAgentRows still
	// renders and shows up as a duplicate. (The global table renders
	// allowed_users under a "[platforms.telegram]" section header, so the
	// dotted key is the agent table's alone.)
	if n := strings.Count(joined, "platforms.telegram.allowed_users"); n != 1 {
		t.Errorf("platforms.telegram.allowed_users appears %d times, want 1:\n%s", n, joined)
	}

	// Unset settings show nothing: a second platform with no operator
	// configuration adds no operator rows.
	pure := FormatConfigGrouped(
		&Config{Platforms: []PlatformConfig{{ID: "discord", Access: AccessConfig{AllowedUsers: []string{"5"}}}}},
		AgentConfig{ID: "x"})
	if strings.Contains(strings.Join(pure, "\n"), "operator_users") {
		t.Errorf("unset operator settings should not be shown:\n%s", strings.Join(pure, "\n"))
	}
}

func TestFormatConfigGroupedShowsAgentDiscordOperatorSettings(t *testing.T) {
	// Proves the AGENT table shows the operator settings of every platform
	// the agent configures — Discord exactly like Telegram, per agent and
	// globally (spec 1) — next to that platform's allowed_users.
	agent := AgentConfig{ID: "scout", Platforms: []PlatformConfig{{
		ID: "discord",
		Access: AccessConfig{
			AllowedUsers:        []string{"222"},
			OperatorUsers:       []string{"888"},
			NonOperatorCommands: []string{"help"},
		},
	}}}

	joined := strings.Join(FormatConfigGrouped(&Config{}, agent), "\n")
	for _, want := range []string{
		"platforms.discord.allowed_users", "[222]",
		"platforms.discord.operator_users", "[888]",
		"platforms.discord.non_operator_commands",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent table missing %q:\n%s", want, joined)
		}
	}
}
