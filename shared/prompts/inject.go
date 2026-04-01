package prompts

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/timeutil"
)

// defaultInjectionNote is the standard context note prepended to all injected messages.
const defaultInjectionNote = "[SYSTEM INJECTION — This is a user-role message sent by the agent host system, NOT by the user. The user has not seen it. If the user already knows about it or you don't want to bother them, respond with `[[NO_RESPONSE]]` and nothing else. Otherwise actively *tell* the user about it and explain (e.g. \"I received a notification that...\", \"The system reports...\"). Do NOT passively comment on or observe the content — either `[[NO_RESPONSE]]` or proactively inform the user.]"

// FormatInjectedMessage wraps a system-injected message with a standard header
// and context note. All injected user-role messages (warnings, wakes, inter-session
// messages, etc.) should use this to provide consistent formatting.
//
// Parameters:
//   - tag: short label for the message type (e.g. "SCHEDULED WAKE", "SYSTEM UPDATE")
//   - when: timestamp of the original event (not injection time)
//   - body: the message content
//   - contextNote: optional override for the default system injection note.
//     If provided, the first value replaces the default note.
//
// The output includes an RFC3339 timestamp and a context note reminding the agent
// that the user hasn't seen this message.
func FormatInjectedMessage(tag string, when time.Time, body string, contextNote ...string) string {
	note := defaultInjectionNote
	if len(contextNote) > 0 {
		note = contextNote[0]
	}

	var b strings.Builder
	b.WriteString(note)
	fmt.Fprintf(&b, "\n[%s @ %s]", tag, timeutil.Format(when))
	if body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	return b.String()
}
