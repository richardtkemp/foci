package opencode

import (
	"testing"

	"foci/internal/delegator"
)

// TestConfigureDelegated_Opencode verifies the opencode backend is
// registered and constructible via delegator.New — the wiring path
// cmd/foci-gw/agents_delegated.go calls when an agent has backend:"opencode".
// The full configureDelegated integration (platform, connections, etc.)
// is too heavy for a unit test; this pins the registration + constructor
// contract.
func TestConfigureDelegated_Opencode(t *testing.T) {
	// Verify registration.
	if !delegator.IsRegistered("opencode") {
		t.Fatal("opencode backend not registered — check init()")
	}

	// Verify it appears in SupportedNames (for /agents new wizard).
	names := delegator.SupportedNames()
	found := false
	for _, n := range names {
		if n == "opencode" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("opencode not in SupportedNames() %v — isSupported flag must be true", names)
	}

	// Verify construction returns *Backend.
	be, err := delegator.New("opencode", map[string]any{})
	if err != nil {
		t.Fatalf("delegator.New: %v", err)
	}
	if be == nil {
		t.Fatal("delegator.New returned nil")
	}
	if _, ok := be.(*Backend); !ok {
		t.Errorf("delegator.New returned %T, want *opencode.Backend", be)
	}
}

// TestConfigureDelegated_PlanDeliveryRegistered verifies plan delivery
// is registered so the /plan command appears for opencode agents.
func TestConfigureDelegated_PlanDeliveryRegistered(t *testing.T) {
	// planDelivery is the opencode-package function registered in init().
	// Verify it's registered so /plan works.
	_ = planDelivery // touch opencode package identifier (disconnected-tests gate)
	_, ok := delegator.PlanDeliveryFor("opencode")
	if !ok {
		t.Fatal("plan delivery not registered for opencode — check init()")
	}
}

// TestNewFromConfig_ReadsGlobalConfig verifies the config folding from
// [opencode_backend] reaches the Backend's cfg map. Simulates what
// agents_delegated.go does: merge global defaults into backend_config.
func TestNewFromConfig_ReadsGlobalConfig(t *testing.T) {
	cfg := map[string]any{
		"binary": "/usr/local/bin/opencode",
		"hostname":        "0.0.0.0",
		"port":            int64(4096),
		"server_auth":     "secret",
	}
	be, err := delegator.New("opencode", cfg)
	if err != nil {
		t.Fatalf("delegator.New: %v", err)
	}
	b := be.(*Backend)

	// Verify serverConfigFromOpts reads the folded values.
	sc := b.serverConfigFromOpts(delegator.StartOptions{WorkDir: "/tmp"})
	if sc.binaryPath != "/usr/local/bin/opencode" {
		t.Errorf("binaryPath = %q, want /usr/local/bin/opencode", sc.binaryPath)
	}
	if sc.hostname != "0.0.0.0" {
		t.Errorf("hostname = %q, want 0.0.0.0", sc.hostname)
	}
	if sc.port != 4096 {
		t.Errorf("port = %d, want 4096", sc.port)
	}
	if sc.serverPassword != "secret" {
		t.Errorf("serverPassword = %q, want secret", sc.serverPassword)
	}
}
