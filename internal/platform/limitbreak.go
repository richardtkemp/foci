package platform

import (
	"strings"
	"unicode"
)

// The /limit_break prefix (alias /lb, #3128; Dick 2026-11-15: "a new command
// /limit-break alias /lb to force through a message past the limit"). A chat
// message whose first word is one of LimitBreakWords runs the rest of its text
// as a normal user turn now, past the account's usage hold; the hold itself
// stays on. The chat surfaces pass such a message to the agent as text (not
// to command dispatch), and the agent strips the word and marks the turn
// (agent.Enqueue).

// LimitBreakWords are the words that start a limit-break message, without
// their leading "/" or ".": the command's name, its alias, and the hyphenated
// spelling Dick used (Telegram cannot register a hyphen, so the command is
// named limit_break; typed text may still use the hyphen).
var LimitBreakWords = []string{"limit_break", "lb", "limit-break"}

// LimitBreakUsage is the reply to a bare /lb when the usage hold is not on.
const LimitBreakUsage = "Usage: /lb <message> (or /limit_break <message>) — run this one message now, past the usage hold. A bare /lb runs every message of yours the hold is holding in this chat now. The hold stays on for everything else."

// LimitBreakNothingHeld is the reply to a bare /lb when the usage hold is on
// but holds no user message of the chat (#3182).
const LimitBreakNothingHeld = "Nothing is held in this chat.\n" + LimitBreakUsage

// LimitBreakReleased is the reply to a bare /lb that released the chat's
// held messages (#3182).
const LimitBreakReleased = "▶️ Your held messages run now, past the usage hold, in the order you sent them. The hold stays on for everything else."

// ParseLimitBreak reports whether text's first whitespace-delimited word is a
// limit-break word with a "/" or "." prefix (any case; leading whitespace
// tolerated, as command dispatch trims). rest is the text after that word,
// trimmed of surrounding whitespace (interior newlines kept); "" for the bare
// form. Only a whole first word matches: "/lbs" and "/lb," do not.
func ParseLimitBreak(text string) (rest string, matched bool) {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	word := trimmed
	if i := strings.IndexFunc(trimmed, unicode.IsSpace); i >= 0 {
		word = trimmed[:i]
	}
	if len(word) < 2 || (word[0] != '/' && word[0] != '.') {
		return "", false
	}
	name := strings.ToLower(word[1:])
	for _, w := range LimitBreakWords {
		if name == w {
			return strings.TrimSpace(trimmed[len(word):]), true
		}
	}
	return "", false
}
