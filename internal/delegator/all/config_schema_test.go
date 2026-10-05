package all_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/delegator"
)

// unreachableConfigKeys are keys a backend reads that no config path can
// supply: BackendConfig has no field for them, so they reach the backend
// only if the gateway adds them. A known gap (docs/BACKENDS.md footnote 48),
// listed so it stays visible and no new one can appear unnoticed (#2178).
var unreachableConfigKeys = map[string][]string{
	"codex": {"api_key", "sandbox"},
}

// TestSpecs_ConfigKeysAreSettable: every backend_config key a backend reads
// is a [agents.backend_config] TOML key, or the one the gateway adds itself.
func TestSpecs_ConfigKeysAreSettable(t *testing.T) {
	settable := backendConfigKeys()
	for _, s := range delegator.Specs() {
		var unreachable []string
		for _, k := range s.ConfigKeys {
			if !slices.Contains(settable, k) && k != delegator.FociVersionConfigKey {
				unreachable = append(unreachable, k)
			}
		}
		slices.Sort(unreachable)
		if want := unreachableConfigKeys[s.Name]; !slices.Equal(unreachable, want) {
			t.Errorf("%s reads backend_config keys no config path supplies: %v, want only the known gap %v. "+
				"Add the key to config.BackendConfig (and ToMap), or stop reading it", s.Name, unreachable, want)
		}
	}
}

// TestBackendConfigKeys_EachHonoured: no [agents.backend_config] key is dead,
// i.e. accepted from TOML but read by no backend and not by the gateway.
func TestBackendConfigKeys_EachHonoured(t *testing.T) {
	for _, k := range backendConfigKeys() {
		honoured := false
		for _, s := range delegator.Specs() {
			honoured = honoured || s.HonoursConfigKey(k)
		}
		if !honoured {
			t.Errorf("backend_config.%s is honoured by no backend: declare it in the Spec.ConfigKeys of the backend that reads it, or delete the field", k)
		}
	}
}

// TestStartOptions_EachFieldRead: every StartOptions field is resolved by the
// DelegatedManager or read by at least one backend, so none is dead.
func TestStartOptions_EachFieldRead(t *testing.T) {
	for _, f := range delegator.StartOptionFields() {
		read := slices.Contains(delegator.ManagerStartFields, f)
		for _, s := range delegator.Specs() {
			read = read || slices.Contains(s.StartFields, f)
		}
		if !read {
			t.Errorf("StartOptions.%s is read by no backend: declare it in the Spec.StartFields of the backend that reads it, or delete the field", f)
		}
	}
}

// backendConfigKeys returns the TOML key of every [agents.backend_config]
// field, in declaration order.
func backendConfigKeys() []string {
	t := reflect.TypeFor[config.BackendConfig]()
	out := make([]string, t.NumField())
	for i := range out {
		out[i], _, _ = strings.Cut(t.Field(i).Tag.Get("toml"), ",")
	}
	return out
}

// familySections names each config family's global TOML section.
var familySections = map[string]string{
	delegator.ConfigFamilyClaudeCode: "[cc_backend]",
	delegator.ConfigFamilyOpencode:   "[opencode_backend]",
}

const (
	schemaDocBegin = "<!-- BEGIN GENERATED CONFIG SCHEMA: internal/delegator/all TestBackendsDoc_ConfigSchemaTable. Edit the Specs, not this block. -->"
	schemaDocEnd   = "<!-- END GENERATED CONFIG SCHEMA -->"
)

// TestBackendsDoc_ConfigSchemaTable keeps the per-backend config tables in
// docs/BACKENDS.md equal to the Specs' declarations. On a mismatch it prints
// the block to paste between the markers.
func TestBackendsDoc_ConfigSchemaTable(t *testing.T) {
	path := filepath.Join(repoRoot, "docs", "BACKENDS.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	want := renderConfigSchema(delegator.Specs())
	i, j := strings.Index(doc, schemaDocBegin), strings.Index(doc, schemaDocEnd)
	if i < 0 || j < i {
		t.Fatalf("%s: generated-config-schema markers missing; add:\n%s%s%s", path, schemaDocBegin, want, schemaDocEnd)
	}
	if got := doc[i+len(schemaDocBegin) : j]; got != want {
		t.Errorf("%s config schema tables are stale; replace the block between the markers with:\n%s", path, want)
	}
}

func renderConfigSchema(specs []delegator.Spec) string {
	var b strings.Builder
	header := func(first string) {
		b.WriteString("\n| " + first + " |")
		sep := "\n|---|"
		for _, s := range specs {
			b.WriteString(" " + s.Name + " |")
			sep += "---|"
		}
		b.WriteString(sep + "\n")
	}
	mark := func(ok bool) string {
		if ok {
			return " ✓ |"
		}
		return " ✗ |"
	}

	header("`backend_config` key")
	// A Spec whose package reads nothing honours exactly the keys the
	// gateway reads for it.
	gateway := delegator.Spec{
		StartFields: []string{"Env"},
		Caps: map[delegator.Capability]delegator.Support{
			delegator.CapPreToolRules: delegator.Yes(),
			delegator.CapStopRules:    delegator.Yes(),
		},
	}
	for _, k := range backendConfigKeys() {
		note := ""
		if gateway.HonoursConfigKey(k) {
			note = " (gateway)"
		}
		b.WriteString("| `" + k + "`" + note + " |")
		for _, s := range specs {
			b.WriteString(mark(s.HonoursConfigKey(k)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n(gateway) = read by the gateway for the backend, not by the backend package; " +
		"`env` reaches a backend that reads `StartOptions.Env`, `pretool_rules` / `stop_rules` one declaring `pretool_rules` / `stop_rules`.\n\n")
	for _, s := range specs {
		if s.ConfigFamily != "" {
			b.WriteString("- " + s.Name + " folds the global `" + familySections[s.ConfigFamily] + "` section into its backend_config (Spec.ConfigFamily).\n")
		}
	}
	for _, s := range specs {
		if gap := unreachableConfigKeys[s.Name]; len(gap) > 0 {
			b.WriteString("- " + s.Name + " also reads `" + strings.Join(gap, "`, `") + "`, which no config key supplies.\n")
		}
	}

	header("`StartOptions` field")
	for _, f := range delegator.StartOptionFields() {
		b.WriteString("| `" + f + "` |")
		manager := slices.Contains(delegator.ManagerStartFields, f)
		for _, s := range specs {
			if manager {
				b.WriteString(" manager |")
				continue
			}
			b.WriteString(mark(slices.Contains(s.StartFields, f)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nmanager = resolved by the DelegatedManager before Start; no backend reads it.\n\n")
	return b.String()
}
