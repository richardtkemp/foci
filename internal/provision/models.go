package provision

import "strings"

// ResolveModelAlias maps short aliases to full model IDs.
// Accepts full model IDs as pass-through. Empty input defaults to sonnet.
func ResolveModelAlias(input string) string {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "opus":
		return "claude-opus-4-6"
	case "sonnet", "":
		return "claude-sonnet-4-6"
	case "haiku":
		return "claude-haiku-4-5-20251001"
	default:
		return input
	}
}
