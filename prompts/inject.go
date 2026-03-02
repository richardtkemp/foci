package prompts

import (
	"fmt"
	"strings"
	"time"
)

// FormatInjectedMessage wraps a system-injected message with a standard header
// and context note. All injected user-role messages (warnings, wakes, inter-session
// messages, etc.) should use this to provide consistent formatting.
//
// Parameters:
//   - tag: short label for the message type (e.g. "SCHEDULED WAKE", "SYSTEM UPDATE")
//   - when: timestamp of the original event (not injection time)
//   - body: the message content
//
// The output includes an RFC3339 timestamp and a context note reminding the agent
// that the user hasn't seen this message.
func FormatInjectedMessage(tag string, when time.Time, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s @ %s]", tag, when.UTC().Format(time.RFC3339))
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	b.WriteString("\n\n[SYSTEM INJECTION — This is a user-role message sent by the agent host system, NOT by the user. The user has not seen it, will not see it, and cannot see it. If you reference this message to the user, you MUST explain what you're talking about, e.g. \"a system message just arrived which says...\"]")
	return b.String()
}
