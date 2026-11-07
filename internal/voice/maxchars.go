package voice

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// TextTooLongError says a voice refused a text over its [[tts]] max_chars
// (#2919). No provider was called. It is a skip, not a fault, so callers report
// it apart from a provider failure.
type TextTooLongError struct {
	Chars int // characters in the text the voice would have received
	Max   int // the voice's maximum
}

func (e *TextTooLongError) Error() string {
	return fmt.Sprintf("text of %d characters is over this voice's maximum of %d", e.Chars, e.Max)
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
