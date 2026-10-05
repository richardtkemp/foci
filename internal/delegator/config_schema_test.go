package delegator

import (
	"slices"
	"testing"
)

func TestHonoursConfigKey(t *testing.T) {
	s := validSpec()
	s.ConfigKeys = []string{"binary"}
	s.StartFields = []string{"Env"}
	s.Caps[CapStopRules] = Yes()
	for key, want := range map[string]bool{
		"binary":        true,  // the backend package's own
		"model":         true,  // the gateway's, for every backend
		"idle_timeout":  true,  // the gateway's, for every backend
		"env":           true,  // reaches the backend as StartOptions.Env, which it reads
		"stop_rules":    true,  // CapStopRules Yes
		"pretool_rules": false, // CapPreToolRules No
		"hostname":      false, // nobody reads it
	} {
		if got := s.HonoursConfigKey(key); got != want {
			t.Errorf("HonoursConfigKey(%q) = %v, want %v", key, got, want)
		}
	}
	s.StartFields = nil
	if s.HonoursConfigKey("env") {
		t.Error(`HonoursConfigKey("env") = true for a backend that never reads StartOptions.Env`)
	}
}

func TestIgnoredConfigKeys(t *testing.T) {
	t.Cleanup(func() {
		registryMu.Lock()
		delete(specs, "fake-schema")
		registryMu.Unlock()
	})
	s := validSpec()
	s.Name = "fake-schema"
	s.ConfigKeys = []string{"binary"}
	Register(s)

	got := IgnoredConfigKeys("fake-schema", []string{"binary", "model", "hostname"})
	if want := []string{"hostname (backend fake-schema does not read it)"}; !slices.Equal(got, want) {
		t.Errorf("IgnoredConfigKeys(delegated) = %q, want %q", got, want)
	}
	for _, api := range []string{"", "api"} {
		got := IgnoredConfigKeys(api, []string{"model"})
		if want := []string{"model (the API transport reads no backend_config)"}; !slices.Equal(got, want) {
			t.Errorf("IgnoredConfigKeys(%q) = %q, want %q", api, got, want)
		}
	}
	if got := IgnoredConfigKeys("not-registered", []string{"hostname"}); got != nil {
		t.Errorf("IgnoredConfigKeys(unregistered) = %q, want nil (name validation reports it)", got)
	}
	if got := IgnoredConfigKeys("fake-schema", nil); got != nil {
		t.Errorf("IgnoredConfigKeys(no keys) = %q, want nil", got)
	}
}
