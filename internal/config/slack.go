package config

import (
	"fmt"
	"strings"
	"time"
)

// Slack credential-key defaults and token shapes. The secrets store holds the
// values; config only ever names the KEYS, so a log line or validation message
// can never carry a token.
const (
	// DefaultSlackBotTokenSecret is the secrets key holding the bot token
	// (xoxb-…) when [platforms.slack].bot_token_secret is not set.
	DefaultSlackBotTokenSecret = "slack.bot_token"
	// DefaultSlackAppTokenSecret is the secrets key holding the app-level
	// token (xapp-…) used for Socket Mode.
	DefaultSlackAppTokenSecret = "slack.app_token"
	// SlackBotTokenPrefix / SlackAppTokenPrefix are the token shapes Slack
	// issues; a value without its prefix is a wrong-paste, not a secret.
	SlackBotTokenPrefix = "xoxb-"
	SlackAppTokenPrefix = "xapp-"
	// DefaultSlackAPIBase is the Slack Web API root.
	DefaultSlackAPIBase = "https://slack.com/api"
)

// Slack I/O timeouts: no Slack call or dial may block forever (a stalled
// endpoint would otherwise hold the session's worker — and the synchronous
// calls in the socket read loop would stall all inbound traffic). Both are
// config-overridable ([platforms.slack] request_timeout / connect_timeout).
const (
	DefaultSlackRequestTimeout = 30 * time.Second
	DefaultSlackConnectTimeout = 10 * time.Second
)

// DefaultSlackUnmappedReplyHold is how long one "not set up here" reply
// quiets an unmapped channel's thread ([platforms.slack]
// unmapped_reply_hold): the notice is per conversation per window, not per
// mention.
const DefaultSlackUnmappedReplyHold = 10 * time.Minute

// BotTokenKey returns the secrets key holding the bot token, applying the
// default. Nil-safe.
func (s *SlackSpecific) BotTokenKey() string {
	if s == nil || s.BotTokenSecret == "" {
		return DefaultSlackBotTokenSecret
	}
	return s.BotTokenSecret
}

// AppTokenKey returns the secrets key holding the app-level token, applying
// the default. Nil-safe.
func (s *SlackSpecific) AppTokenKey() string {
	if s == nil || s.AppTokenSecret == "" {
		return DefaultSlackAppTokenSecret
	}
	return s.AppTokenSecret
}

// APIBaseURL returns the Slack Web API base URL, applying the default.
// Nil-safe.
func (s *SlackSpecific) APIBaseURL() string {
	if s == nil || s.APIBase == "" {
		return DefaultSlackAPIBase
	}
	return strings.TrimRight(s.APIBase, "/")
}

// durationOr is the parse-or-default rule every Slack duration accessor
// shares: a positive Go duration, else the default. The value was validated
// at load; this guards direct construction in tests.
func durationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// RequestTimeoutDuration is the HTTP timeout for every Slack Web API call.
func (s *SlackSpecific) RequestTimeoutDuration() time.Duration {
	if s == nil {
		return DefaultSlackRequestTimeout
	}
	return durationOr(s.RequestTimeout, DefaultSlackRequestTimeout)
}

// ConnectTimeoutDuration is the Socket Mode WebSocket dial + handshake
// timeout. Same fallback rule as RequestTimeoutDuration.
func (s *SlackSpecific) ConnectTimeoutDuration() time.Duration {
	if s == nil {
		return DefaultSlackConnectTimeout
	}
	return durationOr(s.ConnectTimeout, DefaultSlackConnectTimeout)
}

// UnmappedReplyHoldDuration is the per-thread reply-hold window for
// "not set up here" notices in unmapped channels. Same fallback rule as
// RequestTimeoutDuration.
func (s *SlackSpecific) UnmappedReplyHoldDuration() time.Duration {
	if s == nil {
		return DefaultSlackUnmappedReplyHold
	}
	return durationOr(s.UnmappedReplyHold, DefaultSlackUnmappedReplyHold)
}

// slackBlock resolves the EFFECTIVE [platforms.slack] block of a config: the
// configured block, or the zero block when a [[platforms]] id="slack" entry
// exists without one — an entry without a block is a half-configured platform
// whose defaults (credential keys, no mapping) must be checked like any
// explicit block, not waved through. nil only when there is no slack entry at
// all. Every Slack gate reads the block through this one accessor, so the
// block-less shape can never slip between them (#2459 req 1).
func (cfg *Config) slackBlock() *SlackSpecific {
	p := cfg.Platform("slack")
	if p == nil {
		return nil
	}
	if p.Slack == nil {
		return &SlackSpecific{}
	}
	return p.Slack
}

// validateSlack checks the Slack platform block's mapping errors. It runs in
// Load, so a broken mapping is fatal at startup AND in -check-config:
// an empty channel id or agent name, an unknown agent (in channels or as
// default_agent), a channel mapped twice (to two agents, or to the same
// agent twice), a [platforms.slack] block under a [[platforms]] entry whose
// id is not "slack" (the transport reads only the id="slack" entry, so the
// block would silently disable Slack), and a [platforms.slack] block on a
// per-agent entry (the mapping spans agents, so the block is global-only)
// are all config errors. Credential problems are NOT here — they need the
// secrets store and are reported by SlackProblems at Init time.
func (cfg *Config) validateSlack() error {
	// The wrong-parent scan runs even with no id="slack" entry: a Slack
	// block under any other entry is a mistake whatever else the config
	// says (with no slack entry it would disable Slack outright).
	for i := range cfg.Platforms {
		p := &cfg.Platforms[i]
		if p.ID != "slack" && p.Slack != nil {
			return fmt.Errorf("[platforms.slack] block on a [[platforms]] entry with id %q: the block belongs on the id = \"slack\" entry; here it is silently ignored", p.ID)
		}
	}
	s := cfg.slackBlock()
	if s == nil {
		return nil
	}
	agents := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents[a.ID] = true
	}
	if s.DefaultAgent != "" && !agents[s.DefaultAgent] {
		return fmt.Errorf("[platforms.slack] default_agent = %q: no such agent configured", s.DefaultAgent)
	}
	seen := make(map[string]string, len(s.Channels))
	for _, ch := range s.Channels {
		switch {
		case ch.ID == "":
			return fmt.Errorf("[platforms.slack] channels: entry with an empty id")
		case ch.Agent == "":
			return fmt.Errorf("[platforms.slack] channels: channel %q: empty agent", ch.ID)
		case !agents[ch.Agent]:
			return fmt.Errorf("[platforms.slack] channels: channel %q: no such agent %q", ch.ID, ch.Agent)
		}
		if prev, dup := seen[ch.ID]; dup {
			return fmt.Errorf("[platforms.slack] channel %q is mapped to both %q and %q: exactly one agent per channel", ch.ID, prev, ch.Agent)
		}
		seen[ch.ID] = ch.Agent
	}
	return nil
}

// validatePerAgentSlackBlocks rejects a [platforms.slack] block on any
// per-agent [[agents.platforms]] entry, whatever the entry's id: the
// channel → agent mapping spans agents, so the block lives on the global
// entry only, and a per-agent copy (on a "slack" entry or smuggled under
// another platform's) would be silently ignored — the transport reads only
// the global entry.
func (cfg *Config) validatePerAgentSlackBlocks() error {
	for _, a := range cfg.Agents {
		for i := range a.Platforms {
			p := &a.Platforms[i]
			if p.Slack != nil {
				return fmt.Errorf("agents[%s].platforms (%s): the [platforms.slack] block lives on the global [[platforms]] id=\"slack\" entry only (its channel→agent mapping spans agents); a per-agent slack entry carries only access/notify/display", a.ID, p.ID)
			}
		}
	}
	return nil
}

// SlackProblems lists the reasons the Slack platform cannot work at runtime:
// a missing, empty or mis-prefixed credential, or a mapping that answers
// nothing (no default_agent and no channels). Every message names the secret
// KEY, never a value. Empty when no slack platform entry exists; a slack
// entry WITHOUT a [platforms.slack] block gets the zero block's checks (both
// tokens missing, nothing mapped) — the entry declares the platform, so its
// defaults must work, not just its explicit settings. Mapping errors are not
// here — validateSlack rejects those in Load. Called by the slack provider's
// Init (with the live secrets store) and by -check-config (with secrets.toml
// loaded beside the config), so both agree.
func SlackProblems(cfg *Config, secrets SecretGetter) []string {
	s := cfg.slackBlock()
	if s == nil {
		return nil
	}
	var problems []string
	check := func(key, prefix, what string) {
		v := ""
		if secrets != nil {
			v, _ = secrets.Get(key)
		}
		switch {
		case v == "":
			problems = append(problems, fmt.Sprintf("secret %q (%s) is missing or empty", key, what))
		case !strings.HasPrefix(v, prefix):
			problems = append(problems, fmt.Sprintf("secret %q (%s) does not look like a Slack %s (expected %s…)", key, what, what, prefix))
		}
	}
	check(s.BotTokenKey(), SlackBotTokenPrefix, "bot token")
	check(s.AppTokenKey(), SlackAppTokenPrefix, "app token")
	if s.DefaultAgent == "" && len(s.Channels) == 0 {
		problems = append(problems, "no default_agent and no channels mapped: the bot would answer nothing (DMs need default_agent, channel mentions need [[platforms.slack.channels]])")
	}
	return problems
}
