package main

import (
	"errors"
	"testing"
)

// TestDefaultLinkSession (#3011): an automatic re-login has no chat, so the
// link's chat is resolved to the session a keyless send reaches, which lets the
// gate tell a reply there (the code) from a message in another chat (a
// repeat). The app's is the conversation its delivery ladder picks; a chat
// platform's is its default chat; unresolved stays "".
func TestDefaultLinkSession(t *testing.T) {
	appCreate := func(agentID string) (string, error) { return "app:" + agentID + ":conv", nil }
	appFail := func(string) (string, error) { return "", errors.New("app provider not running") }

	if got := defaultLinkSession(&appishConn{}, "clutch", appCreate); got != "app:clutch:conv" {
		t.Errorf("app: %q, want the delivery conversation's session", got)
	}
	if got := defaultLinkSession(&appishConn{}, "clutch", appFail); got != "" {
		t.Errorf("app, unresolved: %q, want empty", got)
	}
	tg := &chatRoutingConn{defaultSessionKey: "telegram:clutch:42"}
	if got := defaultLinkSession(tg, "clutch", func(string) (string, error) {
		t.Fatal("app resolver used for a chat platform")
		return "", nil
	}); got != "telegram:clutch:42" {
		t.Errorf("telegram: %q, want the default chat's session", got)
	}
}
