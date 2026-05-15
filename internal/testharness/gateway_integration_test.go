//go:build integration

package testharness

import (
	"strings"
	"testing"
	"time"
)

// TestHarness_GatewayStartsAndSignalsReady is the smoke test for the L2
// scaffolding: build foci-gw + cc-stub, spawn the gateway against the
// Telegram stub, wait for the "started N agent(s)" log line. If this
// passes, the scaffolding is in good shape and downstream scenario
// tests can build on it.
func TestHarness_GatewayStartsAndSignalsReady(t *testing.T) {
	h := StartGateway(t, HarnessOptions{
		Agents: []AgentSpec{
			{ID: "alpha"},
		},
		ReadyTimeout: 30 * time.Second,
	})

	if h.TelegramStub().URL() == "" {
		t.Error("Telegram stub URL is empty")
	}
	if h.RecorderPath() == "" {
		t.Error("recorder path is empty")
	}
	if !strings.Contains(h.Stderr(), "started 1 agent(s)") {
		t.Errorf("expected 'started 1 agent(s)' in stderr, got:\n%s", h.Stderr())
	}
}
