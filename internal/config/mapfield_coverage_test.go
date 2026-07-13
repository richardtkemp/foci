package config

import (
	"os"
	"path/filepath"
	"testing"
)

// mapFieldCoverageCase bundles everything TestMapFieldSections_RoundTripCoverage
// needs to actually exercise one registered map section: a base fixture valid
// enough for Load() to succeed, a key/value pair valid enough to pass that
// section's own validation (groups/groups.fallbacks require real model
// refs; groups.calls/system.webhooks don't), and accessors that read the
// value back out of the decoded Config, both globally and per-agent.
type mapFieldCoverageCase struct {
	fixture      string
	key, value   string
	global       func(cfg *Config) map[string]string
	agent        func(a AgentConfig) map[string]string
	agentFixture string // like fixture, but for the per-agent variant (needs a [[agents]] block)
}

// mapFieldCoverage is the per-section whitelist this test walks — keyed by
// MapFieldSpec.Section, so config.MapFieldSections() drives which entries
// are required. A section present in mapFieldSpecs but MISSING here fails
// the test loudly instead of silently going unchecked: this is the general
// version of the check that would have caught #1233 (a per-agent extraction
// path existing in the registry/addressability layer but never actually
// wired to populate the decoded struct) on day one.
var mapFieldCoverage = map[string]mapFieldCoverageCase{
	"groups": {
		fixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n",
		key:     "myteam", value: "anthropic/claude-haiku-4-5",
		global:       func(cfg *Config) map[string]string { return cfg.Groups.Groups },
		agent:        func(a AgentConfig) map[string]string { return a.Groups.Groups },
		agentFixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n\n[[agents]]\nid = \"a\"\n",
	},
	"groups.calls": {
		fixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n",
		key:     "summarize-file", value: "fast",
		global:       func(cfg *Config) map[string]string { return cfg.Groups.Calls },
		agent:        func(a AgentConfig) map[string]string { return a.Groups.Calls },
		agentFixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n\n[[agents]]\nid = \"a\"\n",
	},
	"groups.fallbacks": {
		// Fallback keys/values must resolve as models (validateFallbacks) —
		// matchMapField also refuses non-bare keys (see bareTOMLKeyRe), so a
		// raw "developer/model_id" key is impossible here; use [models.*]
		// aliases, the realistic way this section is actually populated.
		fixture: "[models.opus]\nmodel = \"anthropic/claude-opus-4-6\"\n\n[models.haiku]\nmodel = \"anthropic/claude-haiku-4-5\"\n\n[groups]\npowerful = \"opus\"\n",
		key:     "haiku", value: "opus",
		global:       func(cfg *Config) map[string]string { return cfg.Groups.Fallbacks },
		agent:        func(a AgentConfig) map[string]string { return a.Groups.Fallbacks },
		agentFixture: "[models.opus]\nmodel = \"anthropic/claude-opus-4-6\"\n\n[models.haiku]\nmodel = \"anthropic/claude-haiku-4-5\"\n\n[groups]\npowerful = \"opus\"\n\n[[agents]]\nid = \"a\"\n",
	},
	"system.webhooks": {
		fixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n",
		key:     "deploy", value: "deploy.md",
		global:       func(cfg *Config) map[string]string { return cfg.System.Webhooks },
		agent:        func(a AgentConfig) map[string]string { return a.System.Webhooks },
		agentFixture: "[groups]\npowerful = \"anthropic/claude-opus-4-6\"\n\n[[agents]]\nid = \"a\"\n",
	},
}

func TestMapFieldSections_RoundTripCoverage(t *testing.T) {
	sections := MapFieldSections()
	if len(sections) == 0 {
		t.Fatal("MapFieldSections() returned nothing — test can't run")
	}

	for _, section := range sections {
		c, ok := mapFieldCoverage[section]
		if !ok {
			t.Errorf("map section %q has no entry in mapFieldCoverage — add one so its round trip is actually verified (this is exactly the kind of silent gap that let #1233 through undetected)", section)
			continue
		}

		t.Run(section+"/global", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(c.fixture), 0o644)

			field, ok := LookupField(section + "." + c.key)
			if !ok {
				t.Fatalf("LookupField(%q) not found", section+"."+c.key)
			}
			formatted, err := FormatTOMLValue(c.value, field.Type)
			if err != nil {
				t.Fatalf("FormatTOMLValue: %v", err)
			}
			if _, err := SetInFile(path, SetTarget{Section: field.Section, Key: field.Key}, formatted, 0640); err != nil {
				t.Fatalf("SetInFile: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				data, _ := os.ReadFile(path)
				t.Fatalf("Load: %v\n--- resulting file ---\n%s", err, data)
			}
			got := c.global(cfg)
			if got[c.key] != c.value {
				t.Errorf("after write+reload, %s.%s = %q, want %q (full map: %v) — write succeeded but the value never reached the decoded Config", section, c.key, got[c.key], c.value, got)
			}
		})

		t.Run(section+"/agent", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(c.agentFixture), 0o644)

			field, ok := LookupField("agent." + section + "." + c.key)
			if !ok {
				t.Fatalf("LookupField(%q) not found", "agent."+section+"."+c.key)
			}
			if field.Section != "agent" {
				t.Fatalf("LookupField(%q).Section = %q, want agent", "agent."+section+"."+c.key, field.Section)
			}
			formatted, err := FormatTOMLValue(c.value, field.Type)
			if err != nil {
				t.Fatalf("FormatTOMLValue: %v", err)
			}
			if _, err := SetInFile(path, SetTarget{Section: "agents", AgentID: "a", Key: field.Key}, formatted, 0640); err != nil {
				t.Fatalf("SetInFile: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				data, _ := os.ReadFile(path)
				t.Fatalf("Load: %v\n--- resulting file ---\n%s", err, data)
			}
			if len(cfg.Agents) == 0 {
				t.Fatal("fixture has no agents")
			}
			got := c.agent(cfg.Agents[0])
			if got[c.key] != c.value {
				t.Errorf("after write+reload, agent.%s.%s = %q, want %q (full map: %v) — write succeeded but the value never reached the decoded per-agent Config", section, c.key, got[c.key], c.value, got)
			}
		})
	}
}
