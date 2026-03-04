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
	b.WriteString("\n\n[SYSTEM INJECTION — This is a user-role message sent by the agent host system, NOT by the user. The user has not seen it. You MUST either (1) reply with nothing if the user already knows about it, or (2) actively *tell* the user about it (e.g. \"I received a notification that...\", \"The system reports...\"). Do NOT passively comment on or observe the content — either ignore it or proactively inform the user.]")
	return b.String()
}
