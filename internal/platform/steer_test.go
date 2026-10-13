package platform

import (
	"testing"
)

// TestParseSteerPrefix pins the pure prefix parser (#1318): a whole first
// word /steer or /queue (case-insensitive, leading whitespace tolerated)
// selects that delivery mode and yields the remaining text verbatim; the
// bare form matches with empty rest; every other first word is not a prefix.
func TestParseSteerPrefix(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantPref    SteerPreference
		wantRest    string
		wantMatched bool
	}{
		{name: "steer", in: "/steer stop that", wantPref: SteerAlways, wantRest: "stop that", wantMatched: true},
		{name: "queue", in: "/queue also do X", wantPref: SteerNever, wantRest: "also do X", wantMatched: true},
		{name: "case-insensitive upper", in: "/QUEUE x", wantPref: SteerNever, wantRest: "x", wantMatched: true},
		{name: "case-insensitive mixed", in: "/StEeR x", wantPref: SteerAlways, wantRest: "x", wantMatched: true},
		{name: "bare queue", in: "/queue", wantPref: SteerNever, wantRest: "", wantMatched: true},
		{name: "bare queue trailing spaces", in: "/queue   ", wantPref: SteerNever, wantRest: "", wantMatched: true},
		{name: "bare steer", in: "/steer", wantPref: SteerAlways, wantRest: "", wantMatched: true},
		{name: "leading spaces tolerated", in: "  /steer x", wantPref: SteerAlways, wantRest: "x", wantMatched: true},
		{name: "leading tab tolerated", in: "\t/queue x", wantPref: SteerNever, wantRest: "x", wantMatched: true},
		{name: "newline separates the word", in: "/steer\nx", wantPref: SteerAlways, wantRest: "x", wantMatched: true},
		{name: "interior newlines preserved in rest", in: "/queue line1\nline2", wantPref: SteerNever, wantRest: "line1\nline2", wantMatched: true},
		{name: "space before rest removed", in: "/queue   spaced", wantPref: SteerNever, wantRest: "spaced", wantMatched: true},
		{name: "longer word not a prefix", in: "/queued x", wantMatched: false},
		{name: "prefix-sounding word not a prefix", in: "/steerage", wantMatched: false},
		{name: "trailing punctuation not a prefix", in: "/queue, x", wantMatched: false},
		{name: "first word only", in: "say /queue now", wantMatched: false},
		{name: "plain text", in: "hello there", wantMatched: false},
		{name: "empty", in: "", wantMatched: false},
		{name: "whitespace only", in: "   ", wantMatched: false},
		{name: "other command untouched", in: "/status now", wantMatched: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pref, rest, matched := ParseSteerPrefix(tc.in)
			if matched != tc.wantMatched {
				t.Fatalf("matched = %v, want %v", matched, tc.wantMatched)
			}
			if !tc.wantMatched {
				return
			}
			if pref != tc.wantPref {
				t.Errorf("pref = %v, want %v", pref, tc.wantPref)
			}
			if rest != tc.wantRest {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestSteerPreferenceFromWire pins the ONE wire mapping every entry point
// uses (#1318, requirement 3): "steer"/"queue" map to their preference, and
// anything else — "", unknown, casing — degrades to the config default
// rather than erroring (forward compatibility).
func TestSteerPreferenceFromWire(t *testing.T) {
	tests := []struct {
		in   string
		want SteerPreference
	}{
		{"steer", SteerAlways},
		{"queue", SteerNever},
		{"", SteerDefault},
		{"bogus", SteerDefault},
		{"STEER", SteerDefault}, // wire values are lowercase; anything else is unknown
	}
	for _, tc := range tests {
		if got := SteerPreferenceFromWire(tc.in); got != tc.want {
			t.Errorf("SteerPreferenceFromWire(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSteerPrefixUsageExact pins the usage line verbatim: it is spec-pinned
// (#1318) and both bots' bare-prefix replies and /help share it.
func TestSteerPrefixUsageExact(t *testing.T) {
	const want = "Usage: /queue <message> — wait for the current turn to finish; /steer <message> — redirect the agent now."
	if SteerPrefixUsage != want {
		t.Errorf("SteerPrefixUsage = %q, want %q", SteerPrefixUsage, want)
	}
}

// TestMessageQueueIsAddressed pins the receive-path addressing oracle
// (#1318): IsAddressed mirrors Enqueue's Rule 1 exactly, so a bare /steer or
// /queue gets its usage reply only where Enqueue would have delivered a
// message at all. DMs are always addressed; group messages need a mention
// unless require-mention is off or a throttle will buffer them.
func TestMessageQueueIsAddressed(t *testing.T) {
	groupMsg := func(mention bool) QueuedMessage {
		return QueuedMessage{IsGroupChat: true, IsMention: mention, ChatID: 1}
	}
	dmMsg := QueuedMessage{IsGroupChat: false, ChatID: 1}

	tests := []struct {
		name           string
		requireMention bool
		throttle       bool
		msg            QueuedMessage
		want           bool
	}{
		{"dm is addressed", true, false, dmMsg, true},
		{"group mention is addressed", true, false, groupMsg(true), true},
		{"group unmentioned dropped under require-mention", true, false, groupMsg(false), false},
		{"group unmentioned delivered via throttle", true, true, groupMsg(false), true},
		{"group unmentioned delivered without require-mention", false, false, groupMsg(false), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := MessageQueueConfig{RequireMention: tc.requireMention}
			if tc.throttle {
				cfg.Throttle = NewGroupThrottle(0, nil, nil) // non-nil so Rule 1 passes it
			}
			q := NewMessageQueue(cfg)
			defer q.Stop()
			if got := q.IsAddressed(tc.msg); got != tc.want {
				t.Errorf("IsAddressed(%+v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
