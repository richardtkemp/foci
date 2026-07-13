package config

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FieldType describes the expected value type for a settable config field.
type FieldType int

const (
	FieldString   FieldType = iota // bare string, quoted in TOML
	FieldInt                       // integer
	FieldFloat                     // float64
	FieldBool                      // true/false
	FieldDuration                  // Go duration string (e.g. "5m"), quoted in TOML
)

// ConfigField describes a single settable config key.
type ConfigField struct {
	Section     string    // TOML section: "agent_loop", "sessions", etc.
	Key         string    // TOML key within the section
	Type        FieldType // value type
	Description string    // one-line description
	Default     string    // built-in default (the `default` struct tag; "" when none)
	// NeedsRestart is true when a change to this field only takes effect after
	// a full server restart, because the value is captured at startup (copied
	// into an agent/subsystem/listener) rather than reachable by the live
	// config-apply path (cmd/foci-gw/liveapply.go). Derived from the `hot`
	// struct tag: fields an applier covers carry hot:"<when>" (when ∈
	// immediate/turn/session/event — how soon a change is observed). A
	// ",global" suffix (hot:"immediate,global") scopes the tag to the field's
	// GLOBAL registry row, for shared structs whose agent/platform override
	// rows have no live consumer. No tag = restart required, the conservative
	// default for new fields.
	NeedsRestart bool
}

// validHotTags are the allowed `hot` struct tag timing values; enforced by test.
var validHotTags = map[string]bool{"immediate": true, "turn": true, "session": true, "event": true}

// scopeName maps a registry walk's section to the scope vocabulary used by the
// `scope` struct tag: the per-agent walk is "agent", the [[platforms]] walk is
// "platform", and every global section (debug, voice, …) is "global".
func scopeName(section string) string {
	switch section {
	case "agent":
		return "agent"
	case "platforms":
		return "platform"
	default:
		return "global"
	}
}

// scopeAllowed reports whether a field carrying the given `scope` tag should
// emit a registry row for `section`. The tag is an ALLOWLIST of scopes
// ("global", "agent", "platform"); an absent/empty tag allows every scope
// (the default — a field with no `scope` tag is offered at all three).
//
// Use it to stop advertising an override that a field's consumers never read at
// that scope, e.g. `scope:"global"` for a process-global toggle (debug.enable_pprof)
// or `scope:"global,agent"` for a per-agent-but-not-per-platform knob.
func scopeAllowed(tag, section string) bool {
	if tag == "" {
		return true
	}
	want := scopeName(section)
	for _, s := range strings.Split(tag, ",") {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

// hotApplies reports whether a `hot` tag marks the registry row in `section`
// as live-appliable (see ConfigField.NeedsRestart).
func hotApplies(tag, section string) bool {
	timing, scope, _ := strings.Cut(tag, ",")
	if !validHotTags[timing] {
		return false
	}
	if scope == "global" && (section == "agent" || section == "platforms") {
		return false
	}
	return true
}

// Constraint defines validation rules for a config field value.
type Constraint struct {
	Min     *float64 // minimum (inclusive), for FieldInt/FieldFloat
	Max     *float64 // maximum (inclusive), for FieldInt/FieldFloat
	Choices []string // valid values, for FieldString (case-insensitive match)
}

// globalSections maps TOML section names to their Go struct types.
// Used by the reflection-based field registry builder.
var globalSections = map[string]reflect.Type{
	"notify":      reflect.TypeOf(NotifyConfig{}),
	"display":     reflect.TypeOf(DisplayConfig{}),
	"nudge":       reflect.TypeOf(NudgeConfig{}),
	"voice":       reflect.TypeOf(VoiceConfig{}),
	"agent_loop":  reflect.TypeOf(AgentLoopConfig{}),
	"behavior":    reflect.TypeOf(BehaviorConfig{}),
	"sessions":    reflect.TypeOf(SessionsConfig{}),
	"tools":       reflect.TypeOf(ToolsConfig{}),
	"browser":     reflect.TypeOf(BrowserConfig{}),
	"keepalive":   reflect.TypeOf(KeepaliveConfig{}),
	"background":  reflect.TypeOf(BackgroundConfig{}),
	"reflection":  reflect.TypeOf(ReflectionConfig{}),
	"debug":       reflect.TypeOf(DebugConfig{}),
	"logging":     reflect.TypeOf(LoggingConfig{}),
	"memory":      reflect.TypeOf(MemoryConfig{}),
	"http":        reflect.TypeOf(HTTPConfig{}),
	"environment": reflect.TypeOf(EnvironmentConfig{}),
	"anthropic":   reflect.TypeOf(AnthropicConfig{}),
	"platforms":   reflect.TypeOf(PlatformConfig{}),
	"resources":   reflect.TypeOf(ResourcesConfig{}),
}

// configFields and fieldConstraints are populated at init time by reflection
// over struct tags. Fields opt in to the registry via the `desc:"..."` tag.
var (
	configFields     []ConfigField
	fieldConstraints map[string]Constraint
)

func init() {
	configFields, fieldConstraints = buildFieldRegistry()
}

// buildFieldRegistry walks all config struct types and builds the field
// registry and constraint map from struct tags.
func buildFieldRegistry() ([]ConfigField, map[string]Constraint) {
	var fields []ConfigField
	constraints := make(map[string]Constraint)

	// Global sections
	for section, typ := range globalSections {
		walkType(typ, section, "", &fields, constraints)
	}

	// Per-agent sections: walk AgentConfig's named struct fields
	agentType := reflect.TypeOf(AgentConfig{})
	for i := 0; i < agentType.NumField(); i++ {
		f := agentType.Field(i)
		tomlTag := extractTOMLTag(f)
		if tomlTag == "" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		walkType(ft, "agent", tomlTag, &fields, constraints)
	}

	// Sort fields by section then key for deterministic output.
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].Section != fields[j].Section {
			return fields[i].Section < fields[j].Section
		}
		return fields[i].Key < fields[j].Key
	})

	return fields, constraints
}

// walkType recursively walks a struct type, registering fields that have a
// `desc` tag. Named struct fields add a dotted prefix; anonymous (embedded)
// structs contribute their fields without adding a prefix level.
func walkType(typ reflect.Type, section, prefix string, fields *[]ConfigField, constraints map[string]Constraint) {
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)

		// Recurse into embedded (anonymous) struct fields — no prefix added
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walkType(ft, section, prefix, fields, constraints)
			}
			continue
		}

		tomlTag := extractTOMLTag(f)
		if tomlTag == "" {
			continue
		}

		// Named struct field (or pointer to struct) — recurse with dotted prefix
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			newPrefix := tomlTag
			if prefix != "" {
				newPrefix = prefix + "." + tomlTag
			}
			walkType(ft, section, newPrefix, fields, constraints)
			continue
		}

		// Skip slices and maps — not settable via /config set
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Map {
			continue
		}

		// Scalar field — register if it has a desc tag
		desc := f.Tag.Get("desc")
		if desc == "" {
			continue
		}

		// Scope allowlist: skip emitting this scope's row when a `scope` tag
		// excludes it (e.g. a process-global toggle offered only at "global").
		if !scopeAllowed(f.Tag.Get("scope"), section) {
			continue
		}

		fieldType := inferFieldType(f)
		if fieldType < 0 {
			continue // unsupported type
		}

		key := tomlTag
		if prefix != "" {
			key = prefix + "." + tomlTag
		}

		*fields = append(*fields, ConfigField{
			Section:      section,
			Key:          key,
			Type:         fieldType,
			Description:  desc,
			Default:      f.Tag.Get("default"),
			NeedsRestart: !hotApplies(f.Tag.Get("hot"), section),
		})

		// Parse constraints from tags
		if c := parseConstraint(f); c != nil {
			constraints[section+"."+key] = *c
		}
	}
}

// extractTOMLTag returns the TOML key name from a struct field's tag,
// or "" if the field has no usable TOML tag.
func extractTOMLTag(f reflect.StructField) string {
	tag := f.Tag.Get("toml")
	if tag == "" || tag == "-" {
		return ""
	}
	// Handle "name,option" format (e.g. ",inline")
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// inferFieldType determines the FieldType for a struct field from its Go type
// and optional `type` tag override.
func inferFieldType(f reflect.StructField) FieldType {
	if f.Tag.Get("type") == "duration" {
		return FieldDuration
	}

	typ := f.Type
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	switch typ.Kind() {
	case reflect.Bool:
		return FieldBool
	case reflect.Int, reflect.Int64:
		return FieldInt
	case reflect.Float64:
		return FieldFloat
	case reflect.String:
		return FieldString
	default:
		return -1 // unsupported
	}
}

// parseConstraint builds a Constraint from struct field tags (choices, min, max).
// Returns nil if no constraint tags are present.
func parseConstraint(f reflect.StructField) *Constraint {
	choices := f.Tag.Get("choices")
	minStr := f.Tag.Get("min")
	maxStr := f.Tag.Get("max")

	if choices == "" && minStr == "" && maxStr == "" {
		return nil
	}

	c := &Constraint{}
	if choices != "" {
		c.Choices = strings.Split(choices, ",")
	}
	if minStr != "" {
		v, _ := strconv.ParseFloat(minStr, 64)
		c.Min = &v
	}
	if maxStr != "" {
		v, _ := strconv.ParseFloat(maxStr, 64)
		c.Max = &v
	}
	return c
}

// GetConstraint returns the constraint for this field, or nil if unconstrained.
func (f ConfigField) GetConstraint() *Constraint {
	c, ok := fieldConstraints[f.Section+"."+f.Key]
	if !ok {
		return nil
	}
	return &c
}

// ValidateValue checks raw user input against this field's constraint.
// It returns nil if the value is acceptable (or the field has no constraint).
func (f ConfigField) ValidateValue(raw string) error {
	c := f.GetConstraint()
	if c == nil {
		return nil
	}

	if len(c.Choices) > 0 {
		lower := strings.ToLower(raw)
		for _, ch := range c.Choices {
			if strings.ToLower(ch) == lower {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(c.Choices, ", "))
	}

	// Numeric range check (works for both FieldInt and FieldFloat).
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil // type parsing will catch this separately
	}
	if c.Min != nil && v < *c.Min {
		return fmt.Errorf("must be >= %s", formatNum(*c.Min))
	}
	if c.Max != nil && v > *c.Max {
		return fmt.Errorf("must be <= %s", formatNum(*c.Max))
	}
	return nil
}

// ConstraintHint returns a human-readable hint string, e.g. "0-1" or "off, preview, full".
// Returns "" if the field has no constraint.
func (f ConfigField) ConstraintHint() string {
	c := f.GetConstraint()
	if c == nil {
		return ""
	}
	if len(c.Choices) > 0 {
		return strings.Join(c.Choices, ", ")
	}
	if c.Min != nil && c.Max != nil {
		return formatNum(*c.Min) + "\u2013" + formatNum(*c.Max)
	}
	if c.Min != nil {
		return ">= " + formatNum(*c.Min)
	}
	if c.Max != nil {
		return "<= " + formatNum(*c.Max)
	}
	return ""
}

// formatNum formats a float64 as an integer string when possible.
func formatNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// MapFieldSpec describes a map-typed config section addressable through
// /config set as "<Section>.<arbitrary-key>=<value>" — an add-or-update,
// since the key set is user-defined (group names, webhook IDs, ...) and not
// known at compile time. walkType (below) skips map-kind struct fields
// entirely for this reason; MapFieldSpec is the deliberate, explicit
// opt-in for the handful of map fields that need /config set support
// anyway.
//
// Section MAY itself be dotted (e.g. "groups.calls") to address a nested
// TOML table. When multiple specs' sections share a prefix (e.g. "groups",
// "groups.calls", "groups.fallbacks"), longer sections must be checked
// first — matchMapField enforces this. Getting it backwards does NOT corrupt
// the file (verified empirically: BurntSushi/toml tolerates a dotted key
// implicitly opening a table that a LATER explicit [table] header then adds
// more keys to — the write-order this codebase always produces, since
// SetInFile always inserts new keys before any following section header) —
// but it DOES write to the wrong place: matching the bare "groups" prefix
// for a "groups.fallbacks.X" key would insert a dotted "fallbacks.X = ..."
// key directly into [groups]'s own body instead of the semantically-correct
// [groups.fallbacks] table, which is confusing on read and in diffs even
// though it loads fine. Longest-prefix-first avoids that, not a corruption.
//
// Caveat: SetInFile writes a NEW key by creating/appending to an EXPANDED
// [Section] table header (e.g. [system.webhooks]\nfoo = "bar"), not an
// inline table (webhooks = { foo = "bar" }). If a section already exists in
// the file in inline-table form, a /config set write creates a second,
// conflicting [Section] header — TOML rejects the duplicate definition at
// next load. None of this repo's registered map sections use inline-table
// form today; migrate any that do to the expanded form before relying on
// /config set for them.
type MapFieldSpec struct {
	Section     string
	Description string
	ElementType FieldType
}

// mapFieldSpecs is the registry of map-typed config sections settable via
// /config set. Kept deliberately small and explicit (see MapFieldSpec doc) —
// add an entry here, not a struct-tag mechanism, since only a handful of
// config maps need this.
var mapFieldSpecs = []MapFieldSpec{
	{Section: "groups.calls", Description: "Call site → model group override", ElementType: FieldString},
	{Section: "groups.fallbacks", Description: "Model → fallback model on transient error (529/5xx/timeout)", ElementType: FieldString},
	{Section: "groups", Description: "Named model group → model ID (e.g. powerful, fast, cheap, or a custom group name)", ElementType: FieldString},
	{Section: "system.webhooks", Description: "Webhook hook ID → prompt file path (POST /webhook/{agent}/{hookid})", ElementType: FieldString},
}

// bareTOMLKeyRe matches a valid unquoted TOML bare key: letters, digits,
// underscore, dash. Anything else (a literal ".", "/", ":", space, ...)
// requires quoting to write as a flat key — SetInFile never quotes, so
// matchMapField refuses any key outside this set. Group/call-site/hook names
// are conventionally simple slugs so this rarely bites; a raw
// "developer/model_id" string (which routinely contains "/" and often ".")
// used directly as a groups.fallbacks key is the one realistic case it
// blocks — use a [models.*] alias there instead, or edit the file directly.
var bareTOMLKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// matchMapField finds the longest-prefix MapFieldSpec matching lowerSectionKey
// (already-lowercased "section.key...") and returns the synthesized field plus
// the matched spec's Section. ok is false if no spec's section is a prefix, if
// the match leaves no key remainder (e.g. "groups" alone, or "groups.calls"
// alone with no trailing key), or if the remainder isn't a valid bare TOML key
// (see bareTOMLKeyRe) — refusing is safer than SetInFile silently writing an
// unquoted key that changes TOML's parsed meaning (a dotted key nests tables)
// or fails to parse at all (most other special characters).
func matchMapField(lowerSectionKey string) (spec MapFieldSpec, key string, ok bool) {
	bestLen := -1
	for _, s := range mapFieldSpecs {
		prefix := strings.ToLower(s.Section) + "."
		if !strings.HasPrefix(lowerSectionKey, prefix) {
			continue
		}
		rest := lowerSectionKey[len(prefix):]
		if !bareTOMLKeyRe.MatchString(rest) {
			continue // empty, or not a safely-unquoted TOML key
		}
		if len(prefix) > bestLen {
			bestLen = len(prefix)
			spec = s
			key = rest
			ok = true
		}
	}
	return spec, key, ok
}

// MapFieldSections returns the Section of every registered MapFieldSpec, for
// cmd/foci-gw's live-apply wiring to register a rebuild-and-swap applier
// against — kept as a derived accessor (not a hand-copied literal list)
// so the two stay in lockstep; see MapFieldSpec's doc for why these can't
// flow through the normal hot-tag/ConfigField coverage test.
func MapFieldSections() []string {
	sections := make([]string, len(mapFieldSpecs))
	for i, s := range mapFieldSpecs {
		sections[i] = s.Section
	}
	return sections
}

// LookupField finds a field by "section.key" (case-insensitive). Falls back
// to matchMapField for map-typed sections, whose keys are user-defined and
// so can never be enumerated as literal ConfigFields ahead of time.
func LookupField(sectionKey string) (ConfigField, bool) {
	lower := strings.ToLower(sectionKey)
	for _, f := range configFields {
		if strings.ToLower(f.Section+"."+f.Key) == lower {
			return f, true
		}
	}
	if spec, key, ok := matchMapField(lower); ok {
		return ConfigField{
			Section:      spec.Section,
			Key:          key,
			Type:         spec.ElementType,
			Description:  spec.Description,
			NeedsRestart: false, // registerLiveApplyMapSections (cmd/foci-gw) covers every mapFieldSpecs entry
		}, true
	}
	return ConfigField{}, false
}

// FieldSections returns the distinct section names in alphabetical order.
func FieldSections() []string {
	seen := map[string]bool{}
	var sections []string
	for _, f := range configFields {
		if !seen[f.Section] {
			seen[f.Section] = true
			sections = append(sections, f.Section)
		}
	}
	sort.Strings(sections)
	return sections
}

// FieldsInSection returns all fields whose Section matches (case-insensitive).
func FieldsInSection(section string) []ConfigField {
	lower := strings.ToLower(section)
	var result []ConfigField
	for _, f := range configFields {
		if strings.ToLower(f.Section) == lower {
			result = append(result, f)
		}
	}
	return result
}
