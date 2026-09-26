package discord

import (
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/netretry"

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

// The #1954 regressions — a transient boot DNS failure is retried, close 4004
// stops at once — now run in the background: see background_connect_test.go
// (#2043).

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
