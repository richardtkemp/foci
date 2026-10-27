package agent

import (
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
)

// TestCacheTTL_OneResolution pins the shared resolution keepalive and the
// warmth indicator both use: [models.*] cache_ttl, then the backend constant,
// else unknown.
func TestCacheTTL_OneResolution(t *testing.T) {
	t.Parallel()
	mgr := &DelegatedManager{Spec: delegator.Spec{CacheTTL: time.Hour}}
	override := func(string) config.ModelDefaults { return config.ModelDefaults{CacheTTL: "30m"} }

	cases := []struct {
		name   string
		ag     *Agent
		want   time.Duration
		wantOK bool
	}{
		{"models override beats backend", &Agent{DelegatedManager: mgr, ModelDefaultsFn: override}, 30 * time.Minute, true},
		{"backend constant", &Agent{DelegatedManager: mgr}, time.Hour, true},
		{"empty override falls through", &Agent{DelegatedManager: mgr, ModelDefaultsFn: func(string) config.ModelDefaults { return config.ModelDefaults{} }}, time.Hour, true},
		{"nothing knows", &Agent{}, 0, false},
	}
	for _, c := range cases {
		got, ok := c.ag.CacheTTL("m")
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: CacheTTL = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.wantOK)
		}
		// The indicator path must agree whenever the TTL is known.
		if c.wantOK {
			if ind := c.ag.cacheTTLFor("a/c1"); ind != c.want {
				t.Errorf("%s: indicator TTL %v, keepalive TTL %v — they must match", c.name, ind, c.want)
			}
		}
	}
}
