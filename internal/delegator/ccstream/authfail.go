package ccstream

import "strings"

// isAuthFailure reports whether s looks like a Claude Code authentication
// failure — a 401 from the Anthropic API surfaced through the subprocess, e.g.
// "Failed to authenticate. API Error: 401 Invalid authentication credentials".
//
// The token can die mid-session and the error can arrive either as an error
// `result` message (OnResult) or on the subprocess's stderr/exit path, so the
// match is by symptom (substring) and applied at both detection sites. The
// re-login gate single-flights downstream, so duplicate firings are harmless.
func isAuthFailure(s string) bool {
	if s == "" {
		return false
	}
	ls := strings.ToLower(s)
	switch {
	case strings.Contains(ls, "invalid authentication credentials"):
		return true
	case strings.Contains(ls, "failed to authenticate"):
		return true
	case strings.Contains(ls, "401") && strings.Contains(ls, "authenticat"):
		return true
	}
	return false
}

// fireAuthFailure invokes the auth-failure hook if one is registered. Safe to
// call from multiple detection sites.
func (b *Backend) fireAuthFailure(detail string) {
	if b.onAuthFailure != nil {
		b.onAuthFailure(detail)
	}
}
