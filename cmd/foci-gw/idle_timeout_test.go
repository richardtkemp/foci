package main

import (
	"testing"
	"time"

	"foci/internal/config"
)

// TestResolveIdleTimeout proves the #2280 idle-timeout precedence: the agent's
// backend_config.idle_timeout, then [sessions] idle_timeout, then 0 (the
// manager's 3h agent.DefaultIdleTimeout).
func TestResolveIdleTimeout(t *testing.T) {
	agentSet := config.BackendConfig{IdleTimeout: config.Ptr("20m")}
	tests := []struct {
		name   string
		global string
		bc     config.BackendConfig
		want   time.Duration
	}{
		{"agent wins", "1h", agentSet, 20 * time.Minute},
		{"global when agent unset", "1h", config.BackendConfig{}, time.Hour},
		{"default when both unset", "", config.BackendConfig{}, 0},
		{"agent alone", "", agentSet, 20 * time.Minute},
	}
	for _, tt := range tests {
		cfg := &config.Config{Sessions: config.SessionsConfig{IdleTimeout: tt.global}}
		if got := resolveIdleTimeout(cfg, tt.bc); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}
