package configapprove

import (
	"fmt"
	"strings"

	"foci/internal/config"
)

// value.go turns the value text a request carries into the TOML value the
// edit writes (#3199). A value is TOML (`"DEBUG"`, 42, true), but a user
// typing `foci config set logging.level DEBUG` means the string: when the
// path names a registry field of a string type (string, duration, schedule)
// and the text is a bare word that is not TOML, it is quoted for them.
// Anything else that is not TOML fails here with a reason that says how to
// quote a string, before the edit is planned.

// normalizeValue returns value as the TOML text to write for op at p, or an
// error naming what is wrong with it. An unset carries no value.
func normalizeValue(p config.KeyPath, op Op, value string) (string, error) {
	if op == OpUnset {
		return value, nil
	}
	_, err := config.DecodeTOMLValue(value)
	if err == nil {
		return value, nil
	}
	if op == OpSet && bareWord(value) {
		if ft, ok := stringFieldType(p); ok {
			return config.FormatTOMLValue(value, ft)
		}
	}
	if bareWord(value) {
		return "", fmt.Errorf("%w. A string value must be quoted, e.g. '%q' (shell quotes outside, TOML quotes inside)", err, strings.TrimSpace(value))
	}
	return "", err
}

// bareWord reports whether value is unquoted text: not empty and not opening
// a string, array or table, so a malformed quoted value is never re-quoted
// into a string the user did not type.
func bareWord(value string) bool {
	v := strings.TrimSpace(value)
	return v != "" && !strings.ContainsAny(v[:1], `"'[{`)
}

// stringFieldType is the type of the registry field p names, when that field
// holds a string in TOML. A path with an element selector names no registry
// field.
func stringFieldType(p config.KeyPath) (config.FieldType, bool) {
	if p.Elem != nil || len(p.Keys) == 0 {
		return 0, false
	}
	section, key := p.LiveApplyAddress()
	if key == "" {
		return 0, false
	}
	f, ok := config.LookupField(section + "." + key)
	if !ok {
		return 0, false
	}
	switch f.Type {
	case config.FieldString, config.FieldDuration, config.FieldSchedule:
		return f.Type, true
	}
	return 0, false
}
