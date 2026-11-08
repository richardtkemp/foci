package voice

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// TextTooLongError says a voice refused a text over its [[tts]] max_chars
// (#2919). No provider was called. It is a skip, not a fault, so callers report
// it apart from a provider failure.
//
// A provider with its own hard input limit (Google's 5,000 bytes, #2921) also
// returns it, with Unit "bytes", so that limit is a skip too.
type TextTooLongError struct {
	Chars int    // size of the text the voice would have received, in Unit
	Max   int    // the voice's maximum, in Unit
	Unit  string // "" means characters (Unicode code points)
}

func (e *TextTooLongError) Error() string {
	unit := e.Unit
	if unit == "" {
		unit = "characters"
	}
	return fmt.Sprintf("text of %d %s is over this voice's maximum of %d", e.Chars, unit, e.Max)
}

// maxCharsTTS refuses a text over max, before the provider sees it.
type maxCharsTTS struct {
	inner TTS
	max   int
}

// WithMaxChars limits t to max characters (Unicode code points). A max of zero
// or below means no limit and returns t unchanged.
func WithMaxChars(t TTS, max int) TTS {
	if max <= 0 {
		return t
	}
	return &maxCharsTTS{inner: t, max: max}
}

func (m *maxCharsTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if n := utf8.RuneCountInString(text); n > m.max {
		return nil, &TextTooLongError{Chars: n, Max: m.max}
	}
	return m.inner.Synthesize(ctx, text)
}
