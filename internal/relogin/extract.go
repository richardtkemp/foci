package relogin

import "strings"

// Screen anchors emitted by `claude /login` in its interactive TUI. The login
// URL is shown between the sign-in and paste anchors; success is confirmed by
// the success anchor.
const (
	anchorSignIn  = "Use the url below to sign in"
	anchorPaste   = "Paste code here if prompted"
	anchorSuccess = "Login successful"
)

// extractLoginURL pulls the sign-in URL from a captured tmux pane. It isolates
// the text between the sign-in and paste anchors, drops everything that can't
// be part of a URL (whitespace, newlines, TUI box-drawing glyphs the pane
// capture leaves around the URL), and returns the https URL. Returns "" if the
// anchors or a URL aren't present yet (the caller polls until they are).
func extractLoginURL(pane string) string {
	between, ok := sliceBetween(pane, anchorSignIn, anchorPaste)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, r := range between {
		if isURLRune(r) {
			b.WriteRune(r)
		}
	}
	joined := b.String()
	k := strings.Index(joined, "https://")
	if k < 0 {
		return ""
	}
	return joined[k:]
}

// sliceBetween returns the substring strictly between the first occurrence of
// start and the first subsequent occurrence of end. ok is false if either
// anchor is missing.
func sliceBetween(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// isURLRune reports whether r is a character that can legitimately appear in a
// URL (RFC 3986 unreserved + reserved + percent). Everything else — spaces,
// newlines, the │ box glyph the TUI wraps the URL in — is stripped.
func isURLRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", r)
}
