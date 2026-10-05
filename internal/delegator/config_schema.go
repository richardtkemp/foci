package delegator

import (
	"fmt"
	"reflect"
	"slices"
)

// Per-backend config schema (#2178). A backend reads two kinds of input that
// not every backend honours: keys of its [agents.backend_config] map and
// fields of StartOptions. Each Spec declares the ones its own package reads
// (Spec.ConfigKeys, Spec.StartFields), so a key or option a backend ignores is
// visible rather than silently dropped:
//   - the gateway warns about every backend_config key an agent sets that its
//     backend does not honour (Spec.HonoursConfigKey);
//   - `make lint` (scripts/find-backend-capability-bypass) fails when a
//     backend package reads a key or field its Spec does not declare, or
//     declares one it never reads;
//   - the delegator/all tests fail on a backend_config key no backend honours
//     and a StartOptions field no backend reads, and generate the per-backend
//     config table in docs/BACKENDS.md.

// ManagerStartFields are the StartOptions fields the DelegatedManager resolves
// before Start (into SystemPrompt, Effort and Model, and the exec bridge's
// Env). No backend reads them: a backend that re-resolved one would override
// the manager's per-session result. Spec.Validate rejects them in StartFields.
var ManagerStartFields = []string{"SystemPromptFunc", "EffortFunc", "ModelFunc", "ExecRegistry"}

// FociVersionConfigKey is the one backend_config map key the gateway adds
// itself: the foci build version, injected into every backend's cfg map. It is
// not a [agents.backend_config] TOML key.
const FociVersionConfigKey = "foci_version"

// gatewayConfigKeys are the backend_config keys the gateway (not the backend
// package) reads for a delegated agent, each with the condition under which
// it reaches the backend.
var gatewayConfigKeys = map[string]func(Spec) bool{
	// The launch model ladder and the agent's displayed model.
	"model": func(Spec) bool { return true },
	// The DelegatedManager's idle shutdown.
	"idle_timeout": func(Spec) bool { return true },
	// Passed through as StartOptions.Env.
	"env": func(s Spec) bool { return slices.Contains(s.StartFields, "Env") },
	// Installed through HostHooks.
	"pretool_rules": func(s Spec) bool { return s.Supports(CapPreToolRules) },
	"stop_rules":    func(s Spec) bool { return s.Supports(CapStopRules) },
}

// HonoursConfigKey reports whether an agent on this backend gets any effect
// from setting backend_config key: the backend package reads it
// (Spec.ConfigKeys) or the gateway does on its behalf.
func (s Spec) HonoursConfigKey(key string) bool {
	if slices.Contains(s.ConfigKeys, key) {
		return true
	}
	if f, ok := gatewayConfigKeys[key]; ok {
		return f(s)
	}
	return false
}

// StartOptionFields returns the names of every StartOptions field, in
// declaration order.
func StartOptionFields() []string {
	t := reflect.TypeFor[StartOptions]()
	out := make([]string, t.NumField())
	for i := range out {
		out[i] = t.Field(i).Name
	}
	return out
}

// validateSchema reports the ways Spec.ConfigKeys and Spec.StartFields are
// malformed. Whether they are TRUE (match what the package reads) is checked
// by `make lint`, which has the package's source.
func (s Spec) validateSchema(bad func(format string, a ...any)) {
	fields := StartOptionFields()
	seen := map[string]bool{}
	for _, f := range s.StartFields {
		switch {
		case !slices.Contains(fields, f):
			bad("StartFields names %q, which is not a StartOptions field", f)
		case slices.Contains(ManagerStartFields, f):
			bad("StartFields names %q, which the DelegatedManager resolves before Start: read its result instead", f)
		case seen[f]:
			bad("StartFields names %q twice", f)
		}
		seen[f] = true
	}
	seen = map[string]bool{}
	for _, k := range s.ConfigKeys {
		switch {
		case k == "":
			bad("ConfigKeys has an empty key")
		case seen[k]:
			bad("ConfigKeys names %q twice", k)
		}
		seen[k] = true
	}
}

// IgnoredConfigKeys returns the keys in set that an agent on backend gets no
// effect from, with the reason. backend "" or "api" is the API transport,
// which reads no backend_config at all. An unregistered backend returns nil
// (config validation reports the bad name).
func IgnoredConfigKeys(backend string, set []string) []string {
	if len(set) == 0 {
		return nil
	}
	if backend == "" || backend == "api" {
		out := make([]string, len(set))
		for i, k := range set {
			out[i] = fmt.Sprintf("%s (the API transport reads no backend_config)", k)
		}
		return out
	}
	s, ok := SpecFor(backend)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range set {
		if !s.HonoursConfigKey(k) {
			out = append(out, fmt.Sprintf("%s (backend %s does not read it)", k, s.Name))
		}
	}
	return out
}
