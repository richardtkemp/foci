package delegator

import (
	"strconv"
	"strings"
)

// PermissionMode declares one backend-supported /mode choice. Value is the
// backend-native control value; aliases never leak into the wire protocol.
type PermissionMode struct {
	Name        string
	Value       string
	Description string
	Aliases     []string
}

// ResolvePermissionMode accepts the backend's labels, native values, aliases,
// and the numbered choices displayed by /mode, without a CC-specific fallback.
func ResolvePermissionMode(modes []PermissionMode, input string) (PermissionMode, bool) {
	input = strings.ToLower(strings.TrimSpace(input))
	if n, err := strconv.Atoi(input); err == nil && n > 0 && n <= len(modes) {
		return modes[n-1], true
	}
	for _, mode := range modes {
		if input == strings.ToLower(mode.Name) || input == strings.ToLower(mode.Value) {
			return mode, true
		}
		for _, alias := range mode.Aliases {
			if input == strings.ToLower(alias) {
				return mode, true
			}
		}
	}
	return PermissionMode{}, false
}
