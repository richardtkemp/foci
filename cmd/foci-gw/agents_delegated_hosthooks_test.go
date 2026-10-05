package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
)

// hookCatcher is a registered test backend that records the HostHooks the
// gateway hands it.
type hookCatcher struct {
	delegator.Delegator
	hooks *delegator.HostHooks
}

func (h *hookCatcher) SetHostHooks(hh delegator.HostHooks) { *h.hooks = hh }

// registerHookCatcher registers a test-only backend declaring caps Yes (and
// every other capability No, so the Spec is valid and Register logs nothing:
// a logged error would reach other tests' warn hooks), returning where its
// instances record their HostHooks. The registry is process-global, so each
// test uses its own name and a rerun (-count) resets the record.
func registerHookCatcher(name string, caps ...delegator.Capability) *delegator.HostHooks {
	if s, ok := delegator.SpecFor(name); ok {
		be, _ := s.New(nil)
		got := be.(*hookCatcher).hooks
		*got = delegator.HostHooks{}
		return got
	}
	got := &delegator.HostHooks{}
	spec := delegator.Spec{
		Name:         name,
		DisplayName:  name,
		New:          func(map[string]any) (delegator.Delegator, error) { return &hookCatcher{hooks: got}, nil },
		Prototype:    (*hookCatcher)(nil),
		Caps:         map[delegator.Capability]delegator.Support{},
		ModelcapsKey: name,
		LedgerKey:    name,
	}
	for _, c := range delegator.AllCapabilities() {
		spec.Caps[c] = delegator.No("test backend")
	}
	for _, c := range caps {
		spec.Caps[c] = delegator.Yes()
	}
	delegator.Register(spec)
	return got
}

// TestConfigureDelegated_HostHooksFollowSpec drives the real configureDelegated
// and checks the HostHooks every backend instance receives (#2154 Phase 3,
// replacing the *ccstream/*opencode/*codex type switches): the rate-limit
// callbacks always, the rules only where the Spec declares them, and the
// Claude Code re-login only for a backend declaring relogin.
func TestConfigureDelegated_HostHooksFollowSpec(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caps  []delegator.Capability
		rules bool
	}{
		{"test-hosthooks-plain", []delegator.Capability{delegator.CapHostHooks}, false},
		{"test-hosthooks-full", []delegator.Capability{delegator.CapHostHooks, delegator.CapPreToolRules, delegator.CapStopRules, delegator.CapRelogin}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := registerHookCatcher(tc.name, tc.caps...)
			p, shared := wiringFixture(t)
			p.acfg.Backend = tc.name
			ag := &agent.Agent{}
			if _, ok := configureDelegated(ag, p, shared, tc.name, config.BackendConfig{}); !ok {
				t.Fatal("configureDelegated returned ok=false")
			}
			if _, err := ag.DelegatedManager.NewBackend(); err != nil {
				t.Fatalf("NewBackend: %v", err)
			}
			if got.OnAuthFailure == nil || got.OnRateLimitNotice == nil || got.EngageRateLimit == nil {
				t.Errorf("auth/rate-limit hooks not all wired: %+v", *got)
			}
			if (got.PreToolRules != nil) != tc.rules || (got.StopRules != nil) != tc.rules {
				t.Errorf("rules wired = (%v, %v), want %v", got.PreToolRules != nil, got.StopRules != nil, tc.rules)
			}
			if (ag.ReloginTrigger != nil) != tc.rules {
				t.Errorf("ReloginTrigger set = %v, want %v", ag.ReloginTrigger != nil, tc.rules)
			}
		})
	}
}
