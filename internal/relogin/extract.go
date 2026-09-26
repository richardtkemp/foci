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

// extractLoginURL pulls the sign-in URL from a captured tmux pane. Claude Code
// hard-wraps the URL at the pane edge with explicit newlines (so tmux -J can't
// rejoin it), so the URL is reassembled line by line from the first line
// containing "https://": each line is trimmed of edge whitespace and TUI
// box-drawing glyphs, and the URL ends at the first line that is blank or still
// contains a non-URL character after trimming. That second rule is what stops
// the indented prose hint Claude Code prints under the URL ("Hold Shift (Option
// in iTerm2, ...) while selecting ...") from being glued onto the state param
// (#1931) — a URL fragment never contains a space; prose always does.
// Returns "" if the anchors or a URL aren't present yet (the caller polls).
func extractLoginURL(pane string) string {
	between, ok := sliceBetween(pane, anchorSignIn, anchorPaste)
	if !ok {
		return ""
	}
	k := strings.Index(between, "https://")
	if k < 0 {
		return ""
	}
	notURL := func(r rune) bool { return !isURLRune(r) }
	var b strings.Builder
	for _, line := range strings.Split(between[k:], "\n") {
		frag := strings.TrimFunc(line, notURL)
		if frag == "" || strings.IndexFunc(frag, notURL) >= 0 {
			break
		}
		b.WriteString(frag)
	}
	return b.String()
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
// URL (RFC 3986 unreserved + reserved + percent). Spaces, newlines and the │
// box glyph the TUI wraps the URL in are not.
func isURLRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", r)
}
