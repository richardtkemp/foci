package telegram

import (
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestEntitySpan proves entity spans decode from Telegram's UTF-16
// code-unit offsets into byte ranges: a multi-byte rune before the span
// shifts the two coordinate systems apart, so byte slicing (the round-5
// bug) read garbage or cut a rune in half. Out-of-range and rune-splitting
// spans are rejected, never panicked on — Telegram offsets are advisory.
func TestEntitySpan(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		offset    int64
		length    int64
		wantSpan  string
		wantStart int
		wantEnd   int
		wantOK    bool
	}{
		{"ascii", "hi @focibot", 3, 8, "@focibot", 3, 11, true},
		{"cyrillic before the span", "привет @focibot", 7, 8, "@focibot", 13, 21, true},
		{"emoji before the span", "😀 hi @focibot", 6, 8, "@focibot", 8, 16, true},
		{"three-byte bmp runes before the span", "队列 @focibot", 3, 8, "@focibot", 7, 15, true},
		{"non-ascii span (text_mention name)", "Фоци /queue x", 0, 4, "Фоци", 0, 8, true},
		{"emoji span itself", "a 😀 b", 2, 2, "😀", 2, 6, true},
		{"empty span mid-text", "ab", 1, 0, "", 1, 1, true},
		{"empty span at the end", "hi @focibot", 11, 0, "", 11, 11, true},
		{"offset past the end", "hi @focibot", 12, 0, "", 0, 0, false},
		{"span overrunning the end", "hi @focibot", 3, 9, "", 0, 0, false},
		{"negative offset", "hi", -1, 1, "", 0, 0, false},
		{"negative length", "hi", 0, -1, "", 0, 0, false},
		{"offset inside a surrogate pair", "😀 x", 1, 1, "", 0, 0, false},
		{"empty span inside a surrogate pair", "😀 x", 1, 0, "", 0, 0, false},
		{"length ending inside a surrogate pair", "a😀", 0, 2, "", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			span, start, end, ok := entitySpan(tc.text, gotgbot.MessageEntity{Offset: tc.offset, Length: tc.length})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if span != tc.wantSpan || start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("entitySpan = (%q, %d, %d), want (%q, %d, %d)",
					span, start, end, tc.wantSpan, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

// TestEntitySpanMatchesGotgbotParser pins entitySpan against the library's
// own UTF-16 decode (gotgbot.ParseEntity): for every rune-aligned span of
// every text, the two must agree on the span text. A disagreement means
// the hand-rolled code-unit→byte conversion drifted from Telegram's
// coordinate system.
func TestEntitySpanMatchesGotgbotParser(t *testing.T) {
	texts := []string{
		"hi @focibot",
		"привет @focibot",
		"😀 hi @focibot",
		"队列 @focibot",
		"Фоци /queue 😀 x",
	}
	for _, text := range texts {
		var units int64
		for _, r := range text {
			units++
			if r > 0xFFFF {
				units++
			}
		}
		for offset := int64(0); offset <= units; offset++ {
			for length := int64(0); offset+length <= units; length++ {
				e := gotgbot.MessageEntity{Offset: offset, Length: length}
				span, start, end, ok := entitySpan(text, e)
				if !ok {
					// Only rune-aligned spans convert; a cut inside a
					// surrogate pair has no library answer to compare.
					continue
				}
				if want := gotgbot.ParseEntity(text, e).Text; span != want {
					t.Errorf("entitySpan(%q, offset=%d, length=%d) = %q, gotgbot parses %q",
						text, offset, length, span, want)
				}
				if text[start:end] != span {
					t.Errorf("entitySpan(%q, offset=%d, length=%d): text[%d:%d] != span %q",
						text, offset, length, start, end, span)
				}
			}
		}
	}
}
