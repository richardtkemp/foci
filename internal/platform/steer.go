package platform

import (
	"strings"
	"unicode"
)

// SteerPreference is a sender's per-message routing choice, overriding the
// agent's steer_mode config for this envelope only. Surfaces that offer a
// per-message affordance set it — the app's send options (the wire field
// mapped by SteerPreferenceFromWire), HTTP /send's steer field (foci send
// --steer/--queue), and the /steer|/queue chat prefixes on Telegram/Discord
// (ParseSteerPrefix) — everything else leaves SteerDefault.
//
// Lives in platform (not agent) because the preference must cross the
// platform↔agent boundary on QueuedMessage before the agent Envelope exists.
type SteerPreference int

const (
	// SteerDefault follows the agent's steer_mode config.
	SteerDefault SteerPreference = iota

	// SteerAlways steers mid-turn even when steer_mode is off.
	SteerAlways

	// SteerNever queues the message for a fresh turn after the in-flight
	// turn completes. An explicitly-queued message is a turn and nothing
	// else: it is also exempt from the conversational intercepts that would
	// otherwise consume it mid-turn — plan-cancel feedback (#858), pending
	// foci_ask answer capture (#884), and the backend question/elicitation
	// typed-answer intercepts.
	SteerNever
)

// SteerPrefixUsage is the one-line help both bots reply with for a bare
// /steer or /queue (no message text) and /help teaches the prefixes with.
// Pinned verbatim by ticket #1318.
const SteerPrefixUsage = "Usage: /queue <message> — wait for the current turn to finish; /steer <message> — redirect the agent now."

// SteerPreferenceFromWire maps the wire-level steer field ("steer" / "queue" /
// anything else) to a preference; unknown values degrade to the config
// default rather than erroring — forward compatibility with newer clients.
// The single string→preference mapping for every entry point: the app's
// ClientMessage.Steer, HTTP /send's steer field (foci send --steer/--queue,
// FOCI_STEER, deferred sends) and the /steer|/queue chat prefixes
// (ParseSteerPrefix reduces its word to a wire value) — all use the same
// wire strings (#1318).
func SteerPreferenceFromWire(s string) SteerPreference {
	switch s {
	case "steer":
		return SteerAlways
	case "queue":
		return SteerNever
	default:
		return SteerDefault
	}
}

// ParseSteerPrefix reports whether text's first whitespace-delimited word is
// /steer or /queue (case-insensitive, leading whitespace tolerated — matching
// how command dispatch trims). rest is the remainder with the prefix word and
// following whitespace removed, verbatim (interior newlines preserved); ""
// for the bare form, which the caller answers with SteerPrefixUsage and
// delivers nothing (#1318). Only these two whole first words match: /queued,
// /steerage and /queue, are not prefixes, and a prefix anywhere but the first
// word is ignored. The prefixes are the wire values with a slash prepended,
// so the ONE mapping decides which words carry a choice — the word minus its
// slash, lowercased, goes through SteerPreferenceFromWire; there is no
// second string→preference switch anywhere (#1318).
func ParseSteerPrefix(text string) (pref SteerPreference, rest string, matched bool) {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	word := trimmed
	if i := strings.IndexFunc(trimmed, unicode.IsSpace); i >= 0 {
		word = trimmed[:i]
	}
	if len(word) < 2 || word[0] != '/' {
		return SteerDefault, text, false
	}
	if pref = SteerPreferenceFromWire(strings.ToLower(word[1:])); pref == SteerDefault {
		return SteerDefault, text, false
	}
	return pref, strings.TrimLeftFunc(trimmed[len(word):], unicode.IsSpace), true
}
