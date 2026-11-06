package ccstream

import (
	"encoding/json"
	"testing"
)

func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Failed to authenticate. API Error: 401 Invalid authentication credentials", true},
		{"failed to authenticate", true},
		{"Invalid authentication credentials", true},
		{"API Error: 401 authentication failed", true},
		{"", false},
		{"Error: CC process exited unexpectedly: exit status 1", false},
		{"API Error: 429 rate limited", false},
		{"401 not found", false}, // 401 without an authentication mention
		{"some normal assistant reply", false},
	}
	for _, tc := range cases {
		if got := isAuthFailure(tc.in); got != tc.want {
			t.Errorf("isAuthFailure(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestFireAuthFailure(t *testing.T) {
	b := &Backend{}
	// No hook set → no panic.
	b.fireAuthFailure("x")

	var got string
	b.onAuthFailure = func(detail string) { got = detail }
	b.fireAuthFailure("Failed to authenticate")
	if got != "Failed to authenticate" {
		t.Errorf("hook got %q", got)
	}
}

// Frames captured live on 2026-11-06 from CC 2.1.314 and 2.1.318 with an empty
// CLAUDE_CONFIG_DIR and no auth env (trimmed to the fields foci reads).
const (
	loggedOutAssistantFrame = `{"type":"assistant","message":{"id":"7b0b3bfc","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"Not logged in · Please run /login"}]},"parent_tool_use_id":null,"session_id":"s","uuid":"u","error":"authentication_failed","is_api_error_message":true}`
	loggedOutResultFrame    = `{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","num_turns":1,"terminal_reason":"api_error","session_id":"s","uuid":"u"}`
)

// TestIsAuthFailure_LoginReplies: the replies Claude Code sends when only a new
// sign-in helps (strings from the 2.1.318 binary), and text that names /login
// without being such a reply.
func TestIsAuthFailure_LoginReplies(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"Not logged in · Please run /login",
		"Login expired · Please run /login",
		"Login reached its time limit · Please run /login",
		"OAuth token revoked · Please run /login",
		"Please run /login · API Error: 401 something",
	} {
		if !isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"Voice mode requires a Claude.ai account. Please run /login to sign in.",
		"Please run /login and sign in with your Claude.ai account (not Console).",
		"Warning: this sign-in ends between 2026-11-12 and 2026-11-15 (in 5 days at the earliest). Renewing it does not move those dates. Run claude auth login before then, or use claude setup-token for unattended jobs.",
		"Could not refresh your login because another Claude Code process is refreshing it (or exited mid-refresh) · Try again in a minute; if it keeps happening, close other Claude Code windows or sign in again with /login",
	} {
		if isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = true, want false", s)
		}
	}
}

func parseAssistantFrame(t *testing.T, frame string) *AssistantMessage {
	t.Helper()
	var msg AssistantMessage
	if err := json.Unmarshal([]byte(frame), &msg); err != nil {
		t.Fatalf("parse assistant frame: %v", err)
	}
	return &msg
}

// TestOnAssistant_AuthenticationFailedFiresRelogin: the structured error field
// starts the re-login whatever words the reply uses, including a subagent's
// frame (the credential is shared), and stays quiet for an error that a new
// sign-in cannot fix.
func TestOnAssistant_AuthenticationFailedFiresRelogin(t *testing.T) {
	t.Parallel()
	frame := func(errField, text, parent string) string {
		return `{"type":"assistant","message":{"id":"m","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"parent_tool_use_id":` + parent + `,"error":"` + errField + `"}`
	}
	for _, tc := range []struct {
		name, frame string
		want        string
	}{
		{"captured logged-out frame", loggedOutAssistantFrame, "Not logged in · Please run /login"},
		{"wording foci has never seen", frame("authentication_failed", "Sign-in gone · do the new thing", "null"), "Sign-in gone · do the new thing"},
		{"subagent", frame("authentication_failed", "Login expired · Please run /login", `"toolu_1"`), "Login expired · Please run /login"},
		{"api key not fixable by login", frame("authentication_failed", "Invalid API key · Fix external API key", "null"), ""},
		{"gateway not fixable by login", frame("authentication_failed", "Authentication failed · signing in again won't change this — check with your gateway administrator · API Error: 403", "null"), ""},
		{"temporary", frame("authentication_failed", "Authentication error · This may be a temporary network issue, please try again", "null"), ""},
		{"provider credentials", frame("authentication_failed", "x · refresh your AWS credentials (SSO sign-in) and retry", "null"), ""},
		{"other error class", frame("rate_limit", "Not logged in · Please run /login", "null"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			b := &Backend{}
			b.onAuthFailure = func(d string) { got = d }
			b.OnAssistant(parseAssistantFrame(t, tc.frame))
			if got != tc.want {
				t.Errorf("onAuthFailure got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnAssistant_PlainReplyQuotingLoginTextDoesNotFire: an agent discussing the
// message in an ordinary reply carries no error field.
func TestOnAssistant_PlainReplyQuotingLoginTextDoesNotFire(t *testing.T) {
	t.Parallel()
	fired := false
	b := &Backend{}
	b.onAuthFailure = func(string) { fired = true }
	b.OnAssistant(parseAssistantFrame(t, `{"type":"assistant","message":{"id":"m","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"The reply was Not logged in · Please run /login"}]}}`))
	if fired {
		t.Error("onAuthFailure fired for an ordinary assistant reply")
	}
}

// TestOnResult_AuthFailureReadsTheResultNotTheTurnText: the logged-out result
// fires; an unrelated error result does not fire because earlier turn text
// quoted a login message.
func TestOnResult_AuthFailureReadsTheResultNotTheTurnText(t *testing.T) {
	t.Parallel()
	var captured ResultMessage
	if err := json.Unmarshal([]byte(loggedOutResultFrame), &captured); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		turnText string
		msg      ResultMessage
		want     bool
	}{
		{"captured logged-out result", "", captured, true},
		{"result text with turn text", "Earlier text.", captured, true},
		{"error result, turn text quotes login reply", "The reply was Not logged in · Please run /login", ResultMessage{Subtype: "success", IsError: true, Result: "API Error: 529 Overloaded"}, false},
		{"empty result falls back to turn text", "Failed to authenticate. API Error: 401", ResultMessage{Subtype: "error_during_execution", IsError: true}, true},
		{"not an error", "", ResultMessage{Subtype: "success", Result: "Not logged in · Please run /login"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fired := false
			b := &Backend{}
			b.onAuthFailure = func(string) { fired = true }
			applyHandler(b, &testHandler{})
			b.turnMu.Lock()
			b.turnText.WriteString(tc.turnText)
			b.turnMu.Unlock()
			msg := tc.msg
			b.OnResult(&msg)
			if fired != tc.want {
				t.Errorf("onAuthFailure fired = %v, want %v", fired, tc.want)
			}
		})
	}
}
