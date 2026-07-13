package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildFieldRegistryNonEmpty(t *testing.T) {
	// Proves buildFieldRegistry returns a non-empty registry where every entry
	// has Section, Key, and Description populated.
	fields, constraints := buildFieldRegistry()
	if len(fields) == 0 {
		t.Fatal("buildFieldRegistry returned empty fields slice")
	}
	if len(constraints) == 0 {
		t.Fatal("buildFieldRegistry returned empty constraints map")
	}
	for i, f := range fields {
		if f.Section == "" {
			t.Errorf("field %d: empty Section", i)
		}
		if f.Key == "" {
			t.Errorf("field %d: empty Key", i)
		}
		// Descriptions are optional — auto-discovered fields may have none.
	}
}

func TestStringListFieldsEmitted(t *testing.T) {
	// Proves []string fields (e.g. stop_aliases) now appear in the registry as
	// FieldStringList with wire type "string[]" (previously skipped entirely).
	fields, _ := buildFieldRegistry()
	var found *ConfigField
	for i := range fields {
		if fields[i].Key == "stop_aliases" {
			found = &fields[i]
			break
		}
	}
	if found == nil {
		t.Fatal("stop_aliases ([]string) not emitted in the field registry")
	}
	if found.Type != FieldStringList {
		t.Errorf("stop_aliases type = %v, want FieldStringList", found.Type)
	}
	if got := found.Type.TypeName(); got != "string[]" {
		t.Errorf("stop_aliases wire type = %q, want string[]", got)
	}
}

func TestMapEntries(t *testing.T) {
	flat := map[string]string{
		"groups.powerful":        "opus",
		"groups.fast":            "haiku",
		"groups.calls.summarize": "haiku", // nested map section — not a "groups" entry
		"notify.startup_notify":  "false", // unrelated
	}
	groups := MapEntries("groups", flat)
	if len(groups) != 2 || groups["powerful"] != "opus" || groups["fast"] != "haiku" {
		t.Errorf(`MapEntries("groups") = %v, want {powerful:opus, fast:haiku}`, groups)
	}
	if _, leaked := groups["calls.summarize"]; leaked {
		t.Error("nested groups.calls.* leaked into the groups map")
	}
	calls := MapEntries("groups.calls", flat)
	if len(calls) != 1 || calls["summarize"] != "haiku" {
		t.Errorf(`MapEntries("groups.calls") = %v, want {summarize:haiku}`, calls)
	}
}

func TestDescTagsCoverAllSections(t *testing.T) {
	// Proves that every section in globalSections produces at least one field,
	// and that agent-level fields are also generated.
	sections := FieldSections()
	sectionSet := make(map[string]bool, len(sections))
	for _, s := range sections {
		sectionSet[s] = true
	}

	for section := range globalSections {
		if !sectionSet[section] {
			t.Errorf("globalSections has %q but no fields were generated for it", section)
		}
	}

	// Agent section must also be present
	if !sectionSet["agent"] {
		t.Error("no agent-level fields were generated")
	}
}

func TestNoDuplicateFields(t *testing.T) {
	// Proves there are no duplicate section.key entries in the registry.
	seen := make(map[string]bool)
	for _, f := range configFields {
		key := f.Section + "." + f.Key
		if seen[key] {
			t.Errorf("duplicate field: %s", key)
		}
		seen[key] = true
	}
}

func TestLookupField(t *testing.T) {
	// Proves LookupField finds a known field by dotted path, is case-insensitive,
	// and returns false for unknown paths.

	// Known field
	f, ok := LookupField("agent_loop.max_output_tokens")
	if !ok {
		t.Fatal("LookupField(agent_loop.max_output_tokens) returned false")
	}
	if f.Key != "max_output_tokens" || f.Section != "agent_loop" {
		t.Errorf("got section=%q key=%q", f.Section, f.Key)
	}

	// Case insensitive
	f2, ok := LookupField("AGENT_LOOP.MAX_OUTPUT_TOKENS")
	if !ok {
		t.Fatal("LookupField case-insensitive returned false")
	}
	if f2.Key != f.Key {
		t.Error("case-insensitive lookup returned different field")
	}

	// Unknown
	_, ok = LookupField("nonexistent.field")
	if ok {
		t.Error("LookupField should return false for unknown field")
	}
}

func TestLookupField_MapSections(t *testing.T) {
	// Proves LookupField's map-field fallback: arbitrary (not pre-enumerated)
	// keys under a registered map-typed section resolve to a synthesized
	// field, and longer/more specific prefixes win over shorter ones.
	cases := []struct {
		path        string
		wantSection string
		wantKey     string
	}{
		{"groups.myteam", "groups", "myteam"},
		{"groups.powerful", "groups", "powerful"},
		{"groups.calls.summarize-file", "groups.calls", "summarize-file"},
		{"groups.fallbacks.stepfun", "groups.fallbacks", "stepfun"},
		{"system.webhooks.deploy", "system.webhooks", "deploy"},
		// Per-agent overrides ([agents.groups] etc.) route through the SAME
		// "agent" Section every other per-agent field uses.
		{"agent.groups.myteam", "agent", "groups.myteam"},
		{"agent.groups.calls.summarize-file", "agent", "groups.calls.summarize-file"},
		{"agent.groups.fallbacks.stepfun", "agent", "groups.fallbacks.stepfun"},
		{"agent.system.webhooks.deploy", "agent", "system.webhooks.deploy"},
	}
	for _, c := range cases {
		f, ok := LookupField(c.path)
		if !ok {
			t.Errorf("LookupField(%q) returned false", c.path)
			continue
		}
		if f.Section != c.wantSection || f.Key != c.wantKey {
			t.Errorf("LookupField(%q) = section=%q key=%q, want section=%q key=%q",
				c.path, f.Section, f.Key, c.wantSection, c.wantKey)
		}
		if f.NeedsRestart {
			t.Errorf("LookupField(%q).NeedsRestart = true, want false (live-applied)", c.path)
		}
	}

	// A bare map-section prefix with no trailing key isn't addressable.
	// Note "groups.calls" alone IS addressable — it matches the shorter
	// "groups" prefix with key="calls" (a group literally named "calls"),
	// since a bare two-segment path can't tell "address the groups.calls
	// table" from "define a group named calls" apart from the registered
	// prefix list. That's fine in practice: TOML itself forbids a "calls"
	// scalar key coexisting with a [groups.calls] table in the same file, so
	// the ambiguity self-resolves at reload (whichever is actually declared).
	if _, ok := LookupField("groups"); ok {
		t.Error(`LookupField("groups") should return false — no key remainder`)
	}
	if _, ok := LookupField("system.webhooks"); ok {
		t.Error(`LookupField("system.webhooks") should return false — no key remainder`)
	}
}

func TestFieldSections(t *testing.T) {
	// Proves FieldSections returns a deduplicated list of section names that
	// includes all well-known sections.
	sections := FieldSections()
	if len(sections) == 0 {
		t.Fatal("FieldSections() returned empty")
	}

	// Check uniqueness
	seen := map[string]bool{}
	for _, s := range sections {
		if seen[s] {
			t.Errorf("duplicate section: %s", s)
		}
		seen[s] = true
	}

	// Should include well-known sections
	for _, want := range []string{"agent_loop", "agent", "sessions", "tools", "logging"} {
		if !seen[want] {
			t.Errorf("missing expected section %q", want)
		}
	}
}

func TestFieldsInSection(t *testing.T) {
	// Proves FieldsInSection returns only entries for the requested section,
	// is case-insensitive, and returns empty for unknown section names.
	fields := FieldsInSection("agent_loop")
	if len(fields) == 0 {
		t.Fatal("FieldsInSection(agent_loop) returned empty")
	}
	for _, f := range fields {
		if f.Section != "agent_loop" {
			t.Errorf("unexpected section %q in agent_loop results", f.Section)
		}
	}

	// Case insensitive
	fields2 := FieldsInSection("AGENT_LOOP")
	if len(fields2) != len(fields) {
		t.Errorf("case-insensitive returned %d fields vs %d", len(fields2), len(fields))
	}

	// Unknown section
	empty := FieldsInSection("nonexistent")
	if len(empty) != 0 {
		t.Errorf("unknown section returned %d fields", len(empty))
	}
}

func TestValidateValueFloat(t *testing.T) {
	// Proves float fields with [0,1] constraint accept valid fractions and reject
	// values outside the range.
	f, ok := LookupField("sessions.compaction_threshold")
	if !ok {
		t.Fatal("field not found")
	}

	for _, v := range []string{"0", "0.5", "0.8", "1"} {
		if err := f.ValidateValue(v); err != nil {
			t.Errorf("ValidateValue(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"80", "-1", "1.5"} {
		if err := f.ValidateValue(v); err == nil {
			t.Errorf("ValidateValue(%q) = nil, want error", v)
		}
	}
}

func TestValidateValueInt(t *testing.T) {
	// Proves int fields with range constraints accept valid values and reject
	// values outside the range (e.g. port 0 or 70000).
	f, ok := LookupField("http.port")
	if !ok {
		t.Fatal("field not found")
	}

	for _, v := range []string{"1", "8080", "65535"} {
		if err := f.ValidateValue(v); err != nil {
			t.Errorf("ValidateValue(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"0", "70000"} {
		if err := f.ValidateValue(v); err == nil {
			t.Errorf("ValidateValue(%q) = nil, want error", v)
		}
	}
}

func TestValidateValueChoices(t *testing.T) {
	// Proves string fields with choice constraints accept valid values
	// (case-insensitive) and reject unknown values.
	f, ok := LookupField("logging.level")
	if !ok {
		t.Fatal("field not found")
	}

	for _, v := range []string{"DEBUG", "debug", "Info", "WARN", "error"} {
		if err := f.ValidateValue(v); err != nil {
			t.Errorf("ValidateValue(%q) = %v, want nil", v, err)
		}
	}
	if err := f.ValidateValue("verbose"); err == nil {
		t.Error("ValidateValue(\"verbose\") = nil, want error")
	}
}

func TestValidateValueNoConstraint(t *testing.T) {
	// Proves fields without constraints accept any value.
	f, ok := LookupField("sessions.compaction_summary_prompt")
	if !ok {
		t.Fatal("field not found")
	}

	for _, v := range []string{"anything", "123", "true"} {
		if err := f.ValidateValue(v); err != nil {
			t.Errorf("ValidateValue(%q) = %v, want nil", v, err)
		}
	}
}

func TestConstraintHint(t *testing.T) {
	// Proves ConstraintHint returns the correct human-readable hint string
	// for range constraints and choice constraints.
	tests := []struct {
		field string
		want  string
	}{
		{"sessions.compaction_threshold", "0\u20131"},
		{"http.port", "1\u201365535"},
		{"sessions.compaction_max_tokens", ">= 0"},
		{"logging.level", "DEBUG, INFO, WARN, ERROR"},
		{"sessions.compaction_summary_prompt", ""},
	}
	for _, tt := range tests {
		f, ok := LookupField(tt.field)
		if !ok {
			t.Fatalf("field %q not found", tt.field)
		}
		if got := f.ConstraintHint(); got != tt.want {
			t.Errorf("ConstraintHint(%q) = %q, want %q", tt.field, got, tt.want)
		}
	}
}

func TestAllDescTagsRegistered(t *testing.T) {
	// Proves that every struct field with a `desc` tag in the globalSections
	// structs appears in the registry.
	fieldSet := make(map[string]bool)
	for _, f := range configFields {
		fieldSet[f.Section+"."+f.Key] = true
	}

	for section, typ := range globalSections {
		checkDescTags(t, typ, section, "", fieldSet)
	}
}

// checkDescTags recursively checks that every field with a desc tag in typ
// has a corresponding entry in fieldSet.
func checkDescTags(t *testing.T, typ reflect.Type, section, prefix string, fieldSet map[string]bool) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)

		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				checkDescTags(t, ft, section, prefix, fieldSet)
			}
			continue
		}

		tomlTag := f.Tag.Get("toml")
		if tomlTag == "" || tomlTag == "-" {
			continue
		}
		if idx := strings.IndexByte(tomlTag, ','); idx >= 0 {
			tomlTag = tomlTag[:idx]
		}

		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			newPrefix := tomlTag
			if prefix != "" {
				newPrefix = prefix + "." + tomlTag
			}
			checkDescTags(t, ft, section, newPrefix, fieldSet)
			continue
		}

		desc := f.Tag.Get("desc")
		if desc == "" {
			continue
		}

		// A scope tag may legitimately exclude this scope's row (mirrors walkType).
		if !scopeAllowed(f.Tag.Get("scope"), section) {
			continue
		}

		key := tomlTag
		if prefix != "" {
			key = prefix + "." + tomlTag
		}
		fullKey := section + "." + key
		if !fieldSet[fullKey] {
			t.Errorf("field %s has desc tag but is not in registry", fullKey)
		}
	}
}

func TestHotTagValuesValid(t *testing.T) {
	// A typo'd `hot` tag would silently register as needs-restart; catch it here.
	seen := map[string]bool{}
	var walk func(typ reflect.Type)
	walk = func(typ reflect.Type) {
		if seen[typ.String()] {
			return
		}
		seen[typ.String()] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walk(ft)
				continue
			}
			if hot, ok := f.Tag.Lookup("hot"); ok {
				timing, scope, hasScope := strings.Cut(hot, ",")
				if !validHotTags[timing] {
					t.Errorf("field %s.%s has invalid hot timing %q (valid: immediate, turn, session, event)", typ.String(), f.Name, hot)
				}
				if hasScope && scope != "global" {
					t.Errorf("field %s.%s has invalid hot scope %q (valid: global)", typ.String(), f.Name, scope)
				}
			}
		}
	}
	for _, typ := range globalSections {
		walk(typ)
	}
	walk(reflect.TypeOf(AgentConfig{}))
}

func TestScopeAllowlistExcludesRows(t *testing.T) {
	// A `scope` tag is an allowlist of the scopes at which a field emits a
	// registry row. Fields that a consumer never reads at a given scope
	// (#1199) carry scope:"global" or scope:"global,agent" so the config
	// editor stops advertising a knob that does nothing.
	present := []string{
		"debug.enable_pprof",             // scope:"global" — global row kept
		"debug.cache_bust_detect",        // scope:"global,agent"
		"agent.debug.cache_bust_detect",  // agent allowed
		"keepalive.interval",             // no scope tag — all scopes
		"agent.keepalive.interval",       // no scope tag — all scopes
		"tools.max_result_chars",         // no scope tag
	}
	absent := []string{
		"agent.debug.enable_pprof",           // scope:"global" drops agent
		"platforms.debug.enable_pprof",       // and platform
		"agent.voice.max_frame_bytes",        // scope:"global" drops agent
		"agent.permissions.prompt_ttl",       // scope:"global" drops agent
		"platforms.debug.cache_bust_detect",  // scope:"global,agent" drops platform
		"platforms.notify.warning_max_per_window", // scope:"global,agent" drops platform
	}
	for _, k := range present {
		if _, ok := LookupField(k); !ok {
			t.Errorf("field %s should be in the registry", k)
		}
	}
	for _, k := range absent {
		if _, ok := LookupField(k); ok {
			t.Errorf("field %s should be excluded by its scope tag but is in the registry", k)
		}
	}

	// The kept rows keep their hot-ness: the global debug toggles are still live-appliable.
	if f, ok := LookupField("debug.enable_pprof"); !ok || f.NeedsRestart {
		t.Errorf("debug.enable_pprof should be present and hot (NeedsRestart=false)")
	}
}
