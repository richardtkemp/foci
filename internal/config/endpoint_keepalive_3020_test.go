package config

import (
	"strings"
	"testing"
)

const endpointKeepaliveBase = "[[agents]]\nid = \"a\"\n\n[endpoints.local]\nformat = \"openai\"\nurl = \"http://127.0.0.1:11434/v1\"\n"

// TestEndpointKeepalive_Load (#3020): an endpoint's keepalive.enabled loads
// from a sub-table and from a dotted key; only false turns keepalive off, and
// unset leaves it to the agent/global settings.
func TestEndpointKeepalive_Load(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		off  bool
	}{
		{"unset", "", false},
		{"dotted false", "keepalive.enabled = false\n", true},
		{"dotted true", "keepalive.enabled = true\n", false},
		{"sub-table false", "\n[endpoints.local.keepalive]\nenabled = false\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadTOML(t, endpointKeepaliveBase+tc.tail)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Endpoints["local"].KeepaliveOff(); got != tc.off {
				t.Errorf("KeepaliveOff = %v, want %v", got, tc.off)
			}
			if got := EndpointKeepaliveOff(cfg.Endpoints, "local"); got != tc.off {
				t.Errorf("EndpointKeepaliveOff(local) = %v, want %v", got, tc.off)
			}
			// Built-in endpoints and unknown or empty names are never off.
			for _, name := range []string{"anthropic", "nope", ""} {
				if EndpointKeepaliveOff(cfg.Endpoints, name) {
					t.Errorf("EndpointKeepaliveOff(%q) = true, want false", name)
				}
			}
		})
	}
}

// TestEndpointKeepalive_EditorRoundTrip (#3020): the app config editor can set
// keepalive.enabled on an endpoint entry, the written file loads with the
// endpoint off, the editor reads the value back (dotted-key and sub-table
// forms), and unset removes it.
func TestEndpointKeepalive_EditorRoundTrip(t *testing.T) {
	spec := mapObjectSpecFor(t, "endpoints")
	settings, err := ParseMapObjectValue(spec, `{"keepalive.enabled":"false"}`)
	if err != nil {
		t.Fatalf("ParseMapObjectValue: %v", err)
	}
	out, err := PlanSetMapObjectEntry(endpointKeepaliveBase, spec, "local", settings)
	if err != nil {
		t.Fatalf("PlanSetMapObjectEntry: %v", err)
	}
	if !strings.Contains(out, "keepalive.enabled = false") {
		t.Fatalf("written file lacks the key:\n%s", out)
	}
	cfg, err := loadTOML(t, out)
	if err != nil {
		t.Fatalf("load written file: %v\n%s", err, out)
	}
	if !cfg.Endpoints["local"].KeepaliveOff() {
		t.Errorf("written file does not turn keepalive off:\n%s", out)
	}

	for name, content := range map[string]string{
		"dotted":    out,
		"sub-table": endpointKeepaliveBase + "\n[endpoints.local.keepalive]\nenabled = false\n",
	} {
		path := writeMapObjectFixture(t, content)
		entries, err := MapObjectFileEntries(path, spec)
		if err != nil {
			t.Fatalf("%s: MapObjectFileEntries: %v", name, err)
		}
		if got := entries["local"]["keepalive.enabled"]; got != "false" {
			t.Errorf("%s: read back keepalive.enabled = %q, want false (entry %v)", name, got, entries["local"])
		}
	}

	unset, err := PlanUnsetMapObject(out, spec, "local.keepalive.enabled")
	if err != nil {
		t.Fatalf("PlanUnsetMapObject: %v", err)
	}
	if strings.Contains(unset, "keepalive") {
		t.Errorf("unset left the key:\n%s", unset)
	}
}
