package discord

import (
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/netretry"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// withFastGatewayBackoff keeps the production schedule's shape (unbounded)
// but with millisecond delays, so a test that forgets to succeed hangs
// visibly rather than passing.
func withFastGatewayBackoff(t *testing.T) {
	t.Helper()
	orig := gatewayBackoff
	gatewayBackoff = netretry.Backoff{InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2}
	t.Cleanup(func() { gatewayBackoff = orig })
}

// withFakeGateway replaces openGateway with a fake that fails the first
// `failures` calls with err, then connects. Returns the attempt counter.
func withFakeGateway(t *testing.T, failures int, err error) *int {
	t.Helper()
	attempts := 0
	orig := openGateway
	openGateway = func(*discordgo.Session) error {
		attempts++
		if attempts <= failures {
			return err
		}
		return nil
	}
	t.Cleanup(func() { openGateway = orig })
	return &attempts
}

// setupDiscordAgent runs SetupAgent for an agent with a configured discord
// bot token, exactly as provider.SetupAgentConnection does at boot.
func setupDiscordAgent(t *testing.T) *BotManager {
	t.Helper()
	mgr := NewBotManager()
	cfg := &config.Config{}
	acfg := config.AgentConfig{
		ID:        "clutch",
		Platforms: []config.PlatformConfig{{ID: "discord", Bot: "clutch"}},
	}
	SetupAgent(mgr, AgentSetupParams{
		AgentConfig:  acfg,
		GlobalConfig: cfg,
		SecretStore:  testSecretStore(t, "[discord]\nclutch = \"token\"\n"),
		Resolved:     config.Resolve(cfg, acfg),
	})
	return mgr
}

// bootDNSError is the exact failure from the 2026-09-20 / 2026-09-25 reboots:
// foci came up before systemd-resolved was answering.
func bootDNSError() error {
	return &url.Error{Op: "Get", URL: "https://discord.com/api/v9/gateway", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "server misbehaving", Name: "discord.com", Server: "127.0.0.53:53", IsTemporary: true},
	}}
}

// TestSetupAgent_RetriesGatewayThroughBootDNS is the #1954 regression: a
// transient DNS failure opening the gateway must be retried, not leave the
// agent running without discord until the next restart.
func TestSetupAgent_RetriesGatewayThroughBootDNS(t *testing.T) {
	withFastGatewayBackoff(t)
	attempts := withFakeGateway(t, 3, bootDNSError())

	mgr := setupDiscordAgent(t)

	if mgr.PrimaryBot("clutch") == nil {
		t.Fatalf("no discord bot registered after %d gateway attempt(s): transient DNS error was not retried", *attempts)
	}
	if *attempts != 4 {
		t.Errorf("gateway attempts = %d, want 4 (3 transient failures + 1 success)", *attempts)
	}
}

// TestSetupAgent_BadTokenFailsFast: Discord rejects a bad token by closing
// the gateway with 4004 during Open. Retrying cannot fix that, so setup must
// give up on the first attempt.
func TestSetupAgent_BadTokenFailsFast(t *testing.T) {
	withFastGatewayBackoff(t)
	authErr := &websocket.CloseError{Code: 4004, Text: "Authentication failed."}
	attempts := withFakeGateway(t, 99, authErr)

	mgr := setupDiscordAgent(t)

	if mgr.PrimaryBot("clutch") != nil {
		t.Fatal("bot registered despite a rejected token")
	}
	if *attempts != 1 {
		t.Errorf("gateway attempts = %d, want 1 (auth failure must not retry)", *attempts)
	}
}

func TestIsPermanentDiscordErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"boot DNS", bootDNSError(), false},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"gateway 502", errors.New("HTTP 502 Bad Gateway, "), false},
		{"close 4000 unknown error", &websocket.CloseError{Code: 4000, Text: "Unknown error."}, false},
		{"close 4008 rate limited", &websocket.CloseError{Code: 4008, Text: "Rate limited."}, false},
		{"close 4009 session timed out", &websocket.CloseError{Code: 4009, Text: "Session timed out."}, false},
		{"close 4004 bad token", &websocket.CloseError{Code: 4004, Text: "Authentication failed."}, true},
		{"close 4013 invalid intents", &websocket.CloseError{Code: 4013, Text: "Invalid intent(s)."}, true},
		{"close 4014 disallowed intents", &websocket.CloseError{Code: 4014, Text: "Disallowed intent(s)."}, true},
		{"REST 401", errors.New(`HTTP 401 Unauthorized, {"message": "401: Unauthorized", "code": 0}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentDiscordErr(tc.err); got != tc.want {
				t.Errorf("isPermanentDiscordErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
