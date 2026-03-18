package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestFieldsNonEmpty(t *testing.T) {
	// Proves configFields returns a non-empty registry where every entry has Section,
	// Key, and Description populated.
	fields := configFields
	if len(fields) == 0 {
		t.Fatal("configFields returned empty slice")
	}
	for i, f := range fields {
		if f.Section == "" {
			t.Errorf("field %d: empty Section", i)
		}
		if f.Key == "" {
			t.Errorf("field %d: empty Key", i)
		}
		if f.Description == "" {
			t.Errorf("field %d (%s.%s): empty Description", i, f.Section, f.Key)
		}
	}
}

func TestLookupField(t *testing.T) {
	// Proves LookupField finds a known field by dotted path, is case-insensitive,
	// and returns false for unknown paths.

	// Known field
	f, ok := LookupField("llm.model")
	if !ok {
		t.Fatal("LookupField(llm.model) returned false")
	}
	if f.Key != "model" || f.Section != "llm" {
		t.Errorf("got section=%q key=%q", f.Section, f.Key)
	}

	// Case insensitive
	f2, ok := LookupField("LLM.MODEL")
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
	for _, want := range []string{"llm", "defaults", "agent", "sessions", "tools", "logging"} {
		if !seen[want] {
			t.Errorf("missing expected section %q", want)
		}
	}
}

func TestFieldsInSection(t *testing.T) {
	// Proves FieldsInSection returns only entries for the requested section,
	// is case-insensitive, and returns empty for unknown section names.
	fields := FieldsInSection("defaults")
	if len(fields) == 0 {
		t.Fatal("FieldsInSection(defaults) returned empty")
	}
	for _, f := range fields {
		if f.Section != "defaults" {
			t.Errorf("unexpected section %q in defaults results", f.Section)
		}
	}

	// Case insensitive
	fields2 := FieldsInSection("DEFAULTS")
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
	f, ok := LookupField("llm.model")
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
		{"sessions.compaction_threshold", "0–1"},
		{"http.port", "1–65535"},
		{"sessions.compaction_max_tokens", ">= 0"},
		{"logging.level", "DEBUG, INFO, WARN, ERROR"},
		{"llm.model", ""},
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

func TestFieldsMatchStructTags(t *testing.T) {
	// Proves every field registered in configFields corresponds to a real TOML-tagged
	// struct field in the relevant config struct, guarding against registry drift.

	// Map section names to the struct types they represent.
	sectionStructs := map[string]reflect.Type{
		"llm":              reflect.TypeOf(LLMConfig{}),
		"defaults":         reflect.TypeOf(DefaultsConfig{}),
		"agent":            reflect.TypeOf(AgentConfig{}),
		"anthropic":        reflect.TypeOf(AnthropicConfig{}),
		"gemini":           reflect.TypeOf(GeminiConfig{}),
		"openai":           reflect.TypeOf(OpenAIConfig{}),
		"sessions":         reflect.TypeOf(SessionsConfig{}),
		"telegram":         reflect.TypeOf(TelegramConfig{}),
		"tools":            reflect.TypeOf(ToolsConfig{}),
		"logging":          reflect.TypeOf(LoggingConfig{}),
		"memory":           reflect.TypeOf(MemoryConfig{}),
		"keepalive":        reflect.TypeOf(KeepaliveConfig{}),
		"background":       reflect.TypeOf(BackgroundConfig{}),
		"mana":             reflect.TypeOf(ManaConfig{}),
		"memory_formation": reflect.TypeOf(MemoryFormationConfig{}),
		"environment":      reflect.TypeOf(EnvironmentConfig{}),
		"cache":            reflect.TypeOf(CacheConfig{}),
		"usage_warnings":   reflect.TypeOf(ManaWarningsConfig{}),
		"debug":            reflect.TypeOf(DebugConfig{}),
		"database":         reflect.TypeOf(DatabaseConfig{}),
		"http":             reflect.TypeOf(HTTPConfig{}),
	}

	for _, f := range configFields {
		st, ok := sectionStructs[f.Section]
		if !ok {
			t.Errorf("field %s.%s: section %q has no mapped struct", f.Section, f.Key, f.Section)
			continue
		}

		key := f.Key
		// Dotted keys like "keepalive.enabled" need to resolve the nested struct.
		if dotIdx := strings.Index(key, "."); dotIdx >= 0 {
			prefix := key[:dotIdx]
			suffix := key[dotIdx+1:]
			// Find the nested struct by its TOML tag.
			found := false
			for i := 0; i < st.NumField(); i++ {
				tag := st.Field(i).Tag.Get("toml")
				if tag == prefix && st.Field(i).Type.Kind() == reflect.Struct {
					st = st.Field(i).Type
					key = suffix
					found = true
					break
				}
			}
			if !found {
				t.Errorf("field %s.%s: nested struct %q not found in %s", f.Section, f.Key, prefix, st.Name())
				continue
			}
		}

		// Look for the TOML tag in the struct.
		tagFound := false
		for i := 0; i < st.NumField(); i++ {
			tag := st.Field(i).Tag.Get("toml")
			if tag == key {
				tagFound = true
				break
			}
		}
		if !tagFound {
			t.Errorf("field %s.%s: TOML tag %q not found in struct %s", f.Section, f.Key, key, st.Name())
		}
	}
}
