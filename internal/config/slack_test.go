package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slackConfigTOML builds a loadable config with a slack platform: default
// agent helen, channel C1 mapped to helen, both agents existing. head (when
// non-empty) is global config inserted at the TOP of the file (before any
// table header); tail is appended after everything (extra
// [[platforms.slack.channels]] rows).
func slackConfigTOML(head, tail string) string {
	return head + `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "helen"

[[agents]]
id = "bob"

[[platforms]]
id = "slack"
[platforms.slack]
default_agent = "helen"
[[platforms.slack.channels]]
id = "C1"
agent = "helen"
` + tail
}

// loadSlack loads a config body through the real Load path.
func loadSlack(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

// TestSlackValidate_ValidMappingLoads proves a healthy [platforms.slack]
// block loads through the real Load path with its defaults applied (secret
// keys, API base) — the positive control for every rejection below.
func TestSlackValidate_ValidMappingLoads(t *testing.T) {
	cfg, err := loadSlack(t, slackConfigTOML("", ""))
	if err != nil {
		t.Fatalf("valid slack config failed to load: %v", err)
	}
	p := cfg.Platform("slack")
	if p == nil || p.Slack == nil {
		t.Fatal("slack platform entry missing after load")
	}
	if p.Slack.BotTokenKey() != DefaultSlackBotTokenSecret || p.Slack.AppTokenKey() != DefaultSlackAppTokenSecret {
		t.Errorf("default secret keys = %q/%q", p.Slack.BotTokenKey(), p.Slack.AppTokenKey())
	}
	if p.Slack.APIBaseURL() != DefaultSlackAPIBase {
		t.Errorf("default api base = %q", p.Slack.APIBaseURL())
	}
}

// TestSlackValidate_UnknownChannelAgent proves a channel mapped to an agent
// that does not exist is a load error naming both the channel and the agent.
func TestSlackValidate_UnknownChannelAgent(t *testing.T) {
	_, err := loadSlack(t, slackConfigTOML("", `
[[platforms.slack.channels]]
id = "C2"
agent = "ghost"
`))
	if err == nil || !strings.Contains(err.Error(), `channel "C2"`) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("unknown channel agent error = %v, want it to name C2 and ghost", err)
	}
}

// TestSlackValidate_UnknownDefaultAgent proves a default_agent naming no
// configured agent is a load error naming the value.
func TestSlackValidate_UnknownDefaultAgent(t *testing.T) {
	_, err := loadSlack(t, strings.Replace(slackConfigTOML("", ""), `default_agent = "helen"`, `default_agent = "ghost"`, 1))
	if err == nil || !strings.Contains(err.Error(), `default_agent = "ghost"`) {
		t.Errorf("unknown default agent error = %v", err)
	}
}

// TestSlackValidate_EmptyChannelFields proves a channel mapping entry with
// an empty id or an empty agent name is a load error naming which field is
// empty (a half-filled mapping must route nothing silently).
func TestSlackValidate_EmptyChannelFields(t *testing.T) {
	if _, err := loadSlack(t, slackConfigTOML("", "\n[[platforms.slack.channels]]\nid = \"\"\nagent = \"helen\"\n")); err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Errorf("empty channel id error = %v", err)
	}
	if _, err := loadSlack(t, slackConfigTOML("", "\n[[platforms.slack.channels]]\nid = \"C3\"\nagent = \"\"\n")); err == nil || !strings.Contains(err.Error(), "empty agent") {
		t.Errorf("empty channel agent error = %v", err)
	}
}

// TestSlackValidate_ChannelMappedToTwoAgents proves one channel mapped twice
// — to two different agents, or to the same agent twice — is a load error:
// exactly one agent per channel.
func TestSlackValidate_ChannelMappedToTwoAgents(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"two agents", `
[[platforms.slack.channels]]
id = "C1"
agent = "bob"
`, `mapped to both "helen" and "bob"`},
		{"same agent twice", `
[[platforms.slack.channels]]
id = "C1"
agent = "helen"
`, `mapped to both "helen" and "helen"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSlack(t, slackConfigTOML("", tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("duplicate mapping error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestSlackValidate_PerAgentBlockRejected proves a [platforms.slack] block on
// a per-agent [[agents.platforms]] entry is a load error naming the agent:
// the channel→agent mapping spans agents, so the transport reads only the
// global entry and a per-agent block would be silently ignored.
func TestSlackValidate_PerAgentBlockRejected(t *testing.T) {
	body := strings.Replace(slackConfigTOML("", ""),
		"[[agents]]\nid = \"helen\"",
		"[[agents]]\nid = \"helen\"\n\n[[agents.platforms]]\nid = \"slack\"\n[agents.platforms.slack]\ndefault_agent = \"nope\"", 1)
	_, err := loadSlack(t, body)
	if err == nil || !strings.Contains(err.Error(), "agents[helen].platforms (slack)") {
		t.Errorf("per-agent slack block error = %v, want it to name agents[helen].platforms (slack)", err)
	}

	// A per-agent slack entry WITHOUT the block (access/notify/display
	// overrides only) still loads.
	plain := strings.Replace(slackConfigTOML("", ""),
		"[[agents]]\nid = \"helen\"",
		"[[agents]]\nid = \"helen\"\n\n[[agents.platforms]]\nid = \"slack\"\n[agents.platforms.access]\nallowed_users = [\"U1\"]", 1)
	if _, err := loadSlack(t, plain); err != nil {
		t.Errorf("per-agent slack entry without the slack block failed to load: %v", err)
	}
}

// TestSlackValidate_BlockUnderForeignEntry proves a [platforms.slack] block
// on a [[platforms]] entry whose id is not "slack" is a load error naming the
// entry: the transport reads only the id="slack" entry, so the block would
// silently disable Slack (half its settings under one entry, half under
// another). The rejection holds with a real slack entry present AND without
// one (the no-entry shape would disable Slack outright).
func TestSlackValidate_BlockUnderForeignEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"slack entry present", slackConfigTOML("", ""), `entry with id "telegram"`},
		{"no slack entry at all", strings.Replace(slackConfigTOML("", ""), `id = "slack"`, `id = "slack-x"`, 1), `entry with id "slack-x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A telegram-named entry with the slack block smuggled under it.
			body := tc.body + `
[[platforms]]
id = "telegram"
[platforms.telegram]
[platforms.slack]
default_agent = "bob"
`
			_, err := loadSlack(t, body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("slack block under a foreign entry error = %v, want it to name the %s", err, tc.want)
			}
		})
	}
}

// TestSlackValidate_PerAgentBlockUnderForeignID proves a [platforms.slack]
// block smuggled onto a per-agent [[agents.platforms]] entry with a NON-slack
// id is rejected like the id="slack" shape: whatever the entry calls itself,
// the transport never reads a per-agent slack block.
func TestSlackValidate_PerAgentBlockUnderForeignID(t *testing.T) {
	body := strings.Replace(slackConfigTOML("", ""),
		"[[agents]]\nid = \"helen\"",
		"[[agents]]\nid = \"helen\"\n\n[[agents.platforms]]\nid = \"telegram\"\n[agents.platforms.slack]\ndefault_agent = \"nope\"", 1)
	_, err := loadSlack(t, body)
	if err == nil || !strings.Contains(err.Error(), "agents[helen].platforms (telegram)") {
		t.Errorf("per-agent slack block under a foreign id error = %v, want it to name agents[helen].platforms (telegram)", err)
	}
}

// TestSlackValidate_BlocklessEntryGetsDefaultChecks proves a
// [[platforms]] id="slack" entry WITHOUT a [platforms.slack] block is treated
// as a half-configured platform, not as "no slack": it loads (the zero block
// has no mapping errors), SlackProblems reports both default token keys
// missing plus the nothing-mapped problem, and RequiredSecrets names the
// keys — so -check-config and startup's inert log both catch the shape
// instead of waving it through.
func TestSlackValidate_BlocklessEntryGetsDefaultChecks(t *testing.T) {
	body := `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "helen"

[[platforms]]
id = "slack"
`
	cfg, err := loadSlack(t, body)
	if err != nil {
		t.Fatalf("block-less slack entry failed to load: %v", err)
	}
	if cfg.Platform("slack").Slack != nil {
		t.Fatal("premise failed: the configured block should be absent")
	}

	got := SlackProblems(cfg, nil)
	want := []string{
		`secret "slack.bot_token" (bot token) is missing or empty`,
		`secret "slack.app_token" (app token) is missing or empty`,
		"no default_agent and no channels mapped",
	}
	if len(got) != len(want) {
		t.Fatalf("block-less problems = %v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("problem %d = %q, want it to mention %q", i, got[i], w)
		}
	}

	keys := map[string]bool{}
	for _, ref := range RequiredSecrets(cfg) {
		keys[ref.Key] = true
	}
	if !keys[DefaultSlackBotTokenSecret] || !keys[DefaultSlackAppTokenSecret] {
		t.Errorf("block-less required secrets lack the slack tokens: %v", keys)
	}
}

// TestSlackValidate_TimeoutDurationsAreValidated proves the duration-typed
// keys are checked as durations at load (type:"duration" walk), so a typo
// cannot silently fall back to the default.
func TestSlackValidate_TimeoutDurationsAreValidated(t *testing.T) {
	for _, key := range []string{"request_timeout", "connect_timeout", "unmapped_reply_hold"} {
		body := strings.Replace(slackConfigTOML("", ""),
			"[platforms.slack]\ndefault_agent = \"helen\"",
			"[platforms.slack]\ndefault_agent = \"helen\"\n"+key+" = \"fast\"", 1)
		_, err := loadSlack(t, body)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error = %v, want a duration validation error naming the key", key, err)
		}
	}
}

// TestSlackTimeoutDefaults pins the documented defaults as LITERALS (30s,
// 10s, 10m — a self-comparing test could not catch a changed constant) and
// the configured override, so no Slack call or dial can block forever.
func TestSlackTimeoutDefaults(t *testing.T) {
	var zero *SlackSpecific
	if got := zero.RequestTimeoutDuration(); got != 30*time.Second {
		t.Errorf("nil request timeout = %v, want the documented 30s default", got)
	}
	if got := zero.ConnectTimeoutDuration(); got != 10*time.Second {
		t.Errorf("nil connect timeout = %v, want the documented 10s default", got)
	}
	if got := zero.UnmappedReplyHoldDuration(); got != 10*time.Minute {
		t.Errorf("nil unmapped reply hold = %v, want the documented 10m default", got)
	}
	s := &SlackSpecific{RequestTimeout: "1s", ConnectTimeout: "2s", UnmappedReplyHold: "3m"}
	if got := s.RequestTimeoutDuration(); got != time.Second {
		t.Errorf("request timeout = %v, want 1s", got)
	}
	if got := s.ConnectTimeoutDuration(); got != 2*time.Second {
		t.Errorf("connect timeout = %v, want 2s", got)
	}
	if got := s.UnmappedReplyHoldDuration(); got != 3*time.Minute {
		t.Errorf("unmapped reply hold = %v, want 3m", got)
	}
	// Non-positive values fall back to the default (load rejects them; the
	// guard is for direct construction).
	bad := &SlackSpecific{RequestTimeout: "-1s", ConnectTimeout: "0s", UnmappedReplyHold: "0s"}
	if got := bad.RequestTimeoutDuration(); got != DefaultSlackRequestTimeout {
		t.Errorf("negative request timeout = %v, want the default", got)
	}
	if got := bad.ConnectTimeoutDuration(); got != DefaultSlackConnectTimeout {
		t.Errorf("zero connect timeout = %v, want the default", got)
	}
	if got := bad.UnmappedReplyHoldDuration(); got != DefaultSlackUnmappedReplyHold {
		t.Errorf("zero unmapped reply hold = %v, want the default", got)
	}
}

// TestSlackProblems proves the credential oracle: missing/empty/mis-prefixed
// secrets and a nothing-mapped mapping, custom keys honoured, healthy
// configs clean — and no message ever carries a token VALUE.
func TestSlackProblems(t *testing.T) {
	secretsWith := func(kv ...string) fakeSecrets {
		m := fakeSecrets{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	healthy := func(mutate func(*SlackSpecific)) *Config {
		cfg, err := loadSlack(t, slackConfigTOML("", ""))
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(cfg.Platform("slack").Slack)
		}
		return cfg
	}

	if got := SlackProblems(&Config{}, nil); got != nil {
		t.Errorf("no slack platform: problems = %v, want none", got)
	}
	if got := SlackProblems(healthy(nil), secretsWith(DefaultSlackBotTokenSecret, "xoxb-v", DefaultSlackAppTokenSecret, "xapp-v")); len(got) != 0 {
		t.Errorf("healthy: problems = %v, want none", got)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*SlackSpecific)
		secrets SecretGetter
		want    []string // one full expected problem each, exact count
	}{
		{"missing bot secret", nil, secretsWith(DefaultSlackAppTokenSecret, "xapp-v"),
			[]string{`secret "slack.bot_token" (bot token) is missing or empty`}},
		{"missing app secret", nil, secretsWith(DefaultSlackBotTokenSecret, "xoxb-v"),
			[]string{`secret "slack.app_token" (app token) is missing or empty`}},
		{"empty bot value", nil, secretsWith(DefaultSlackBotTokenSecret, "", DefaultSlackAppTokenSecret, "xapp-v"),
			[]string{`secret "slack.bot_token" (bot token) is missing or empty`}},
		{"wrong bot prefix", nil, secretsWith(DefaultSlackBotTokenSecret, "nope", DefaultSlackAppTokenSecret, "xapp-v"),
			[]string{`secret "slack.bot_token" (bot token) does not look like a Slack bot token (expected xoxb-…)`}},
		{"wrong app prefix", nil, secretsWith(DefaultSlackBotTokenSecret, "xoxb-v", DefaultSlackAppTokenSecret, "nope"),
			[]string{`secret "slack.app_token" (app token) does not look like a Slack app token (expected xapp-…)`}},
		{"nil secrets store", nil, nil, []string{
			`secret "slack.bot_token" (bot token) is missing or empty`,
			`secret "slack.app_token" (app token) is missing or empty`,
		}},
		{"nothing mapped", func(s *SlackSpecific) { s.DefaultAgent = ""; s.Channels = nil },
			secretsWith(DefaultSlackBotTokenSecret, "xoxb-v", DefaultSlackAppTokenSecret, "xapp-v"),
			[]string{"no default_agent and no channels mapped"}},
		{"custom keys", func(s *SlackSpecific) { s.BotTokenSecret = "slack.custom"; s.AppTokenSecret = "slack.custom2" },
			secretsWith("slack.custom", "xoxb-v", "slack.custom2", "xapp-v"), nil},
		{"custom keys missing", func(s *SlackSpecific) { s.BotTokenSecret = "slack.custom"; s.AppTokenSecret = "slack.custom2" }, nil, []string{
			`secret "slack.custom" (bot token) is missing or empty`,
			`secret "slack.custom2" (app token) is missing or empty`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SlackProblems(healthy(tc.mutate), tc.secrets)
			if len(got) != len(tc.want) {
				t.Fatalf("problems = %v, want %d entries", got, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("problem %d = %q, want it to mention %q", i, got[i], want)
				}
			}
		})
	}

	// No message ever carries a token value. The values must be WRONG-prefix
	// ones carrying a marker: valid-shaped tokens produce no problems at all,
	// and a leak check with nothing to inspect can never fire.
	leaky := SlackProblems(healthy(nil), secretsWith(DefaultSlackBotTokenSecret, "SECRETVALUE-bot", DefaultSlackAppTokenSecret, "SECRETVALUE-app"))
	if len(leaky) != 2 {
		t.Fatalf("wrong-prefix fixture: problems = %v, want 2 (one per token; the leak check needs messages to inspect)", leaky)
	}
	for _, p := range leaky {
		if strings.Contains(p, "SECRETVALUE") {
			t.Errorf("problem leaks a token value: %q", p)
		}
	}
}

// fakeSecrets (the package's shared in-memory SecretGetter, validate_test.go)
// backs the secrets arguments below.

// TestDefaultPlatformSlackRejected proves default_platform = "slack" is a
// load error at both the global and the per-agent scope, with a scope label
// in the message (never an empty scope collapsing to a leading space).
func TestDefaultPlatformSlackRejected(t *testing.T) {
	if _, err := loadSlack(t, slackConfigTOML("default_platform = \"slack\"\n", "")); err == nil || !strings.Contains(err.Error(), "never default chats") {
		t.Errorf("global default_platform=slack error = %v", err)
	} else if !strings.Contains(err.Error(), "global default_platform = \"slack\"") || strings.Contains(err.Error(), "config:  ") {
		t.Errorf("global default_platform=slack error = %v, want the global scope label and no double space", err)
	}
	// Per-agent scope: the key sits inside the FIRST [[agents]] block.
	agentScoped := strings.Replace(slackConfigTOML("", ""),
		"[[agents]]\nid = \"helen\"", "[[agents]]\nid = \"helen\"\ndefault_platform = \"slack\"", 1)
	if _, err := loadSlack(t, agentScoped); err == nil || !strings.Contains(err.Error(), "never default chats") {
		t.Errorf("agent default_platform=slack error = %v", err)
	}
	// An ordinary platform still passes the existing check.
	if _, err := loadSlack(t, slackConfigTOML("default_platform = \"slackx\"\n", "")); err == nil || !strings.Contains(err.Error(), "no such [[platforms]] entry") {
		t.Errorf("unknown default_platform error = %v, want the no-such-entry message", err)
	}
}

// TestRequiredSecrets_SlackTokens proves the two default token keys appear
// in RequiredSecrets when a slack platform is configured, and not otherwise.
func TestRequiredSecrets_SlackTokens(t *testing.T) {
	cfg, err := loadSlack(t, slackConfigTOML("", ""))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, ref := range RequiredSecrets(cfg) {
		keys[ref.Key] = true
	}
	if !keys[DefaultSlackBotTokenSecret] || !keys[DefaultSlackAppTokenSecret] {
		t.Errorf("required secrets lack the slack tokens: %v", keys)
	}

	// No slack platform at all: the whole [[platforms]] section comes off
	// (renaming the entry's id would leave the [platforms.slack] block under
	// a foreign entry — a config error of its own).
	cfgNoSlack, err := loadSlack(t, strings.SplitN(slackConfigTOML("", ""), "[[platforms]]", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range RequiredSecrets(cfgNoSlack) {
		if strings.HasPrefix(ref.Key, "slack.") {
			t.Errorf("slack secret %q required without a slack platform", ref.Key)
		}
	}
}
