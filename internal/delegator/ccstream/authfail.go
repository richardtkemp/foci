package ccstream

import (
	"encoding/json"
	"strings"
	"time"
)

// isAuthFailure reports whether s looks like a Claude Code authentication
// failure: a 401 from the Anthropic API surfaced through the subprocess, e.g.
// "Failed to authenticate. API Error: 401 Invalid authentication credentials",
// or a reply that sends the user to /login, e.g. "Not logged in · Please run
// /login" (#2863). Those replies have a fixed "<state> · Please run /login"
// form, which other text naming /login ("Please run /login to sign in") lacks.
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
	case strings.Contains(ls, "· please run /login"), strings.Contains(ls, "please run /login ·"):
		return true
	}
	return false
}

// assistantErrAuthFailed is the top-level error of an assistant frame whose
// credentials were refused. CC sets it on every such reply, whatever the words,
// so it survives CC rewording a reply (#2863).
const assistantErrAuthFailed = "authentication_failed"

// notFixedByLogin lists what CC says, under assistantErrAuthFailed, when a new
// sign-in would not help: a key or provider credential the user supplied, a
// gateway's refusal, a passing network fault (strings from the CC 2.1.318
// binary). A wording missing here costs a needless sign-in link. Reading only
// the login replies instead would cost agents stalled with no link, so this
// lists the exceptions.
var notFixedByLogin = []string{
	"fix external api key",
	"signing in again won't",
	"temporary network issue",
	"refresh your",
	"refresh the gateway token",
}

// assistantAuthFailure returns the reply text of an assistant frame that reports
// a credential only a new sign-in can fix. A subagent's frame counts: it uses
// the same credential as the main thread.
func assistantAuthFailure(msg *AssistantMessage) (string, bool) {
	if msg.Error == nil || *msg.Error != assistantErrAuthFailed {
		return "", false
	}
	var text strings.Builder
	for _, block := range msg.Message.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	detail := strings.TrimSpace(text.String())
	ls := strings.ToLower(detail)
	for _, phrase := range notFixedByLogin {
		if strings.Contains(ls, phrase) {
			return "", false
		}
	}
	if detail == "" {
		detail = assistantErrAuthFailed
	}
	return detail, true
}

// fireAuthFailure invokes the auth-failure hook if one is registered. Safe to
// call from multiple detection sites.
func (b *Backend) fireAuthFailure(detail string) {
	if b.onAuthFailure != nil {
		b.onAuthFailure(detail)
	}
}

// credentialExpiry is a sign-in end date from Claude Code (#2864). It decodes
// without error whatever the field holds: a missing or unreadable date only
// means no warning, and must not lose the frame or fail readiness.
type credentialExpiry struct {
	at time.Time // zero when there is no readable date
}

// UnmarshalJSON implements json.Unmarshaler. It never returns an error.
func (c *credentialExpiry) UnmarshalJSON(data []byte) error {
	c.at = time.Time{}
	var s string
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	if at, err := time.Parse(time.RFC3339, s); err == nil {
		c.at = at
	}
	return nil
}

// reportLoginExpiry passes a sign-in end date to the gateway. A date of zero
// reports nothing.
func (b *Backend) reportLoginExpiry(c credentialExpiry) {
	if b.onLoginExpiry != nil && !c.at.IsZero() {
		b.onLoginExpiry(c.at)
	}
}
