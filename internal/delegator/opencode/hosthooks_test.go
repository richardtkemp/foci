package opencode

import (
	"testing"

	"foci/internal/delegator"
)

// TestSetHostHooks_AuthFailure: opencode relays a provider auth failure to
// HostHooks.OnAuthFailure (#2154 Phase 3; it was SetOnAuthFailure behind a
// *opencode.Backend type assertion in the gateway). EngageRateLimit is covered
// by the session.status tests in ratelimit_test.go.
func TestSetHostHooks_AuthFailure(t *testing.T) {
	b := &Backend{}
	var got string
	b.SetHostHooks(delegator.HostHooks{OnAuthFailure: func(d string) { got = d }})
	b.fireAuthFailure("provider rejected the key")
	if got != "provider rejected the key" {
		t.Errorf("OnAuthFailure got %q", got)
	}
}
