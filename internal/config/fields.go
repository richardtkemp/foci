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
	FieldString     FieldType = iota // bare string, quoted in TOML
	FieldInt                         // integer
	FieldFloat                       // float64
	FieldBool                        // true/false
	FieldDuration                    // Go duration string (e.g. "5m"), quoted in TOML
	FieldStringList                  // []string; wire value is a JSON array, TOML value is ["a", "b"]
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
	"notify":            reflect.TypeOf(NotifyConfig{}),
	"display":           reflect.TypeOf(DisplayConfig{}),
	"nudge":             reflect.TypeOf(NudgeConfig{}),
	"voice":             reflect.TypeOf(VoiceConfig{}),
	"agent_loop":        reflect.TypeOf(AgentLoopConfig{}),
	"behavior":          reflect.TypeOf(BehaviorConfig{}),
	"sessions":          reflect.TypeOf(SessionsConfig{}),
	"tools":             reflect.TypeOf(ToolsConfig{}),
	"browser":           reflect.TypeOf(BrowserConfig{}),
	"keepalive":         reflect.TypeOf(KeepaliveConfig{}),
	"background":        reflect.TypeOf(BackgroundConfig{}),
	"reflection":        reflect.TypeOf(ReflectionConfig{}),
	"debug":             reflect.TypeOf(DebugConfig{}),
	"logging":           reflect.TypeOf(LoggingConfig{}),
	"memory":            reflect.TypeOf(MemoryConfig{}),
	"http":              reflect.TypeOf(HTTPConfig{}),
	"environment":       reflect.TypeOf(EnvironmentConfig{}),
	"platforms":         reflect.TypeOf(PlatformConfig{}),
	"resources":         reflect.TypeOf(ResourcesConfig{}),
	"cc_backend":        reflect.TypeOf(CCBackendConfig{}),
	"opencode_backend":  reflect.TypeOf(OpencodeBackendConfig{}),
	"askgw":             reflect.TypeOf(AskgwConfig{}),
	"bitwarden":         reflect.TypeOf(BitwardenConfig{}),
	"skills":            reflect.TypeOf(SkillsConfig{}),
	"scheduler":         reflect.TypeOf(SchedulerConfig{}),
	"maintenance":       reflect.TypeOf(MaintenanceConfig{}),
	"permissions":       reflect.TypeOf(PermissionsConfig{}),
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

		// Maps are not settable via /config set (they need dynamic-key handling,
		// see mapFieldSpecs). Slices of strings ARE settable (FieldStringList);
		// other slice element types are not yet — inferFieldType returns -1 for
		// them below and they're skipped there.
		if f.Type.Kind() == reflect.Map {
			continue
		}
		if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() != reflect.String {
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
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.String {
			return FieldStringList
		}
		return -1 // other slice element types not settable yet
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

// ObjectSubField describes one sub-key of an object-list entry (a column of a
// [[section]] array-of-tables block).
type ObjectSubField struct {
	Key         string
	Type        FieldType
	Description string
}

// ObjectFieldSpec describes a []struct config section addressable as a TOML
// array-of-tables ([[section]]). Like MapFieldSpec, this is a deliberate,
// explicit opt-in: walkType skips []struct fields, and the whole list is
// replaced atomically by SetTableArray (there is no per-element set path — the
// client always sends the entire array). Section MAY be dotted
// ("memory.sources" -> [[memory.sources]]).
type ObjectFieldSpec struct {
	Section     string
	Description string
	Fields      []ObjectSubField
	Live        bool // if true, NeedsRestart=false in the schema (has a live applier)
}

// objectFieldSpecs is the registry of []struct config sections settable via the
// app config editor as array-of-tables. Kept small and explicit, mirroring
// mapFieldSpecs — the sub-field shapes here must match the corresponding Go
// struct's toml-tagged fields (MessageTransform, BlockedPath, MemorySource).
var objectFieldSpecs = []ObjectFieldSpec{
	{Section: "message_transforms", Description: "Regex find/replace rules applied to inbound messages", Fields: []ObjectSubField{
		{Key: "find", Type: FieldString, Description: "Regex pattern to match"},
		{Key: "replace", Type: FieldString, Description: "Replacement string (supports $1, $2, ...)"},
	}},
	{Section: "blocked_paths", Description: "Path prefixes that write/edit tools refuse (with rebuke message)", Fields: []ObjectSubField{
		{Key: "path", Type: FieldString, Description: "Directory or file prefix to block"},
		{Key: "rebuke", Type: FieldString, Description: "Message returned when a write/edit is attempted"},
	}},
	{Section: "memory.sources", Description: "Memory search index sources (combined additively across all listed dirs)", Fields: []ObjectSubField{
		{Key: "name", Type: FieldString, Description: "Unique identifier (e.g. canonical, code, docs)"},
		{Key: "dir", Type: FieldString, Description: "Directory path to index"},
		{Key: "weight", Type: FieldFloat, Description: "Weight multiplier 0.0-1.0 (1.0 = highest priority)"},
	}},
	{Section: "modelinfo", Description: "Model registry overrides — pricing, capabilities, and context window for models not in the built-in registry, or partial overrides of existing ones", Live: true, Fields: []ObjectSubField{
		{Key: "id", Type: FieldString, Description: "Bare model ID (e.g. 'claude-haiku-4-5'). Overrides existing entry if present, creates new one if not"},
		{Key: "context_window", Type: FieldInt, Description: "Context window in tokens (required for new models)"},
		{Key: "can_effort", Type: FieldBool, Description: "Supports output_config.effort"},
		{Key: "can_thinking", Type: FieldBool, Description: "Supports thinking/reasoning"},
		{Key: "can_speed", Type: FieldBool, Description: "Supports fast mode"},
		{Key: "can_caching", Type: FieldBool, Description: "Supports explicit prompt caching"},
		{Key: "input_per_1m", Type: FieldFloat, Description: "Cost per 1M input tokens in USD (required for new models)"},
		{Key: "output_per_1m", Type: FieldFloat, Description: "Cost per 1M output tokens in USD (required for new models)"},
		{Key: "cache_read_per_1m", Type: FieldFloat, Description: "Cost per 1M cache-read tokens in USD"},
		{Key: "cache_write_per_1m", Type: FieldFloat, Description: "Cost per 1M cache-write tokens in USD"},
	}},
}

// ObjectFields returns the registered settable []struct sections (a copy, so
// callers can't mutate the registry).
func ObjectFields() []ObjectFieldSpec {
	out := make([]ObjectFieldSpec, len(objectFieldSpecs))
	copy(out, objectFieldSpecs)
	return out
}

// ObjectFieldSpecFor returns the spec whose Section matches (case-insensitive),
// or ok=false when section is not a registered object-list.
func ObjectFieldSpecFor(section string) (ObjectFieldSpec, bool) {
	lower := strings.ToLower(section)
	for _, s := range objectFieldSpecs {
		if strings.ToLower(s.Section) == lower {
			return s, true
		}
	}
	return ObjectFieldSpec{}, false
}

// MapObjectFieldSpec describes a map[string]Struct config section — named
// entries (like MapFieldSpec) where each entry has typed sub-fields (like
// ObjectFieldSpec). Section is the TOML table prefix (e.g. "models" for
// [models.powerful]); each entry is a sub-table with the Fields shape.
// Used for [models.*] and [endpoints.*].
type MapObjectFieldSpec struct {
	Section     string
	Description string
	Fields      []ObjectSubField
}

// mapObjectFieldSpecs is the registry of map[string]Struct config sections
// surfaced in the app config schema as named entries with typed sub-fields.
var mapObjectFieldSpecs = []MapObjectFieldSpec{
	{Section: "models", Description: "Named model definitions with per-model settings", Fields: []ObjectSubField{
		{Key: "model", Type: FieldString, Description: "Developer/model ID (e.g. anthropic/claude-sonnet-4-20250514)"},
		{Key: "endpoint", Type: FieldString, Description: "Explicit endpoint override (empty = auto-select from developer)"},
		{Key: "thinking", Type: FieldString, Description: "Thinking mode: adaptive, off"},
		{Key: "effort", Type: FieldString, Description: "Effort level: low, medium, high"},
		{Key: "speed", Type: FieldString, Description: "Speed hint: fast or empty"},
		{Key: "context", Type: FieldString, Description: "Context window size (e.g. 262000 or 262k)"},
		{Key: "enable_keepalive", Type: FieldBool, Description: "Enable cache keepalive for this model"},
		{Key: "cache_ttl", Type: FieldDuration, Description: "Cache TTL (Go duration, empty = auto-detect)"},
		{Key: "cache_strategy", Type: FieldString, Description: "Cache marker strategy: auto or explicit"},
	}},
	{Section: "endpoints", Description: "Named API endpoint definitions", Fields: []ObjectSubField{
		{Key: "format", Type: FieldString, Description: "Wire format: anthropic, openai, or gemini"},
		{Key: "url", Type: FieldString, Description: "Base URL (empty = SDK default)"},
		{Key: "anthropic_url", Type: FieldString, Description: "Anthropic-format URL override"},
		{Key: "openai_url", Type: FieldString, Description: "OpenAI-format URL override"},
		{Key: "gemini_url", Type: FieldString, Description: "Gemini-format URL override"},
		{Key: "api_key", Type: FieldString, Description: "Secret name for API key (e.g. openrouter.api_key)"},
		{Key: "http_timeout", Type: FieldDuration, Description: "HTTP timeout for this endpoint (default 120s)"},
	}},
}

// MapObjectFields returns the registered map[string]Struct sections (a copy).
func MapObjectFields() []MapObjectFieldSpec {
	out := make([]MapObjectFieldSpec, len(mapObjectFieldSpecs))
	copy(out, mapObjectFieldSpecs)
	return out
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
// matchMapField also recognizes an "agent."-prefixed path (e.g.
// "agent.groups.calls.summarize-file") as the per-agent override of a
// registered global map section, per docs/CONFIG.md's "[agents.groups]
// overrides [groups] per-agent" convention (same override shape every other
// per-agent field already uses). It reuses the SAME global specs rather than
// a second, hand-duplicated set — matching them after stripping "agent." —
// and returns Section="agent" (not "agent.groups.calls" or similar): that's
// what ConfigSetDirect's existing `section == "agent"` branch already
// expects for every per-agent override, so this needs ZERO changes there.
// The synthesized Key becomes "<matched-global-section>.<key>" (e.g.
// "groups.calls.summarize-file"), which SetInFile's agent-block writer then
// routes to [agents.<matched-global-section>] (setInAgentBlock's dotted-key
// "upgrade" — see its doc).
const agentMapPrefix = "agent."

func matchMapField(lowerSectionKey string) (spec MapFieldSpec, key string, ok bool) {
	target := lowerSectionKey
	agentScoped := false
	if rest, cut := strings.CutPrefix(lowerSectionKey, agentMapPrefix); cut {
		target = rest
		agentScoped = true
	}

	bestLen := -1
	var bestSpec MapFieldSpec
	var bestKey string
	for _, s := range mapFieldSpecs {
		prefix := strings.ToLower(s.Section) + "."
		if !strings.HasPrefix(target, prefix) {
			continue
		}
		rest := target[len(prefix):]
		if !bareTOMLKeyRe.MatchString(rest) {
			continue // empty, or not a safely-unquoted TOML key
		}
		if len(prefix) > bestLen {
			bestLen = len(prefix)
			bestSpec = s
			bestKey = rest
		}
	}
	if bestLen < 0 {
		return MapFieldSpec{}, "", false
	}
	if !agentScoped {
		return bestSpec, bestKey, true
	}
	return MapFieldSpec{
			Section:     "agent",
			Description: bestSpec.Description,
			ElementType: bestSpec.ElementType,
		},
		bestSpec.Section + "." + bestKey,
		true
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

// MapFieldInfo describes a settable map section for the app config schema.
type MapFieldInfo struct {
	Section     string
	Description string
}

// MapFields returns the registered settable map sections (section + description)
// so the app config schema can advertise them as editable key→value maps. The
// scalar field registry (walkType) skips maps; this is the parallel accessor
// the editor uses to render them.
func MapFields() []MapFieldInfo {
	out := make([]MapFieldInfo, len(mapFieldSpecs))
	for i, s := range mapFieldSpecs {
		out[i] = MapFieldInfo{Section: s.Section, Description: s.Description}
	}
	return out
}

// MapEntries extracts the direct entries of map section `section` from a
// flattened "section.key"→value map (as produced by ExplicitFileValues):
// entryKey→value for every key that is a DIRECT child of section (one segment
// past the section prefix). The direct-child rule keeps nested map sections
// separate — e.g. MapEntries("groups", flat) yields "powerful"→"..." but not
// the "groups.calls.*" entries, which belong to the "groups.calls" section.
func MapEntries(section string, flat map[string]string) map[string]string {
	prefix := section + "."
	out := map[string]string{}
	for k, v := range flat {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || strings.Contains(rest, ".") {
			continue // not under this section, or belongs to a nested map section
		}
		out[rest] = v
	}
	return out
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
