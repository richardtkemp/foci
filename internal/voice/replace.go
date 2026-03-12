package voice

import (
	"context"
	"strings"
	"unicode"
)

// ApplyReplacements performs case-insensitive word replacements on text.
// Each key in replacements is matched as a whole word (bounded by non-letter
// characters or string edges). The replacement preserves the case pattern of
// the matched word: all-upper → all-upper, title-case → title-case, otherwise
// the replacement is used as-is.
func ApplyReplacements(text string, replacements map[string]string) string {
	if len(replacements) == 0 {
		return text
	}

	// Build a lowercase lookup for case-insensitive matching.
	lower := make(map[string]string, len(replacements))
	for k, v := range replacements {
		lower[strings.ToLower(k)] = v
	}

	var b strings.Builder
	b.Grow(len(text))

	i := 0
	for i < len(text) {
		// Skip non-letter characters.
		if !isLetter(text[i]) {
			b.WriteByte(text[i])
			i++
			continue
		}

		// Find end of word.
		j := i + 1
		for j < len(text) && isLetter(text[j]) {
			j++
		}
		word := text[i:j]

		if repl, ok := lower[strings.ToLower(word)]; ok {
			b.WriteString(matchCase(word, repl))
		} else {
			b.WriteString(word)
		}
		i = j
	}

	return b.String()
}

// isLetter reports whether b is an ASCII letter. For simplicity we only handle
// ASCII — the word-boundary logic treats everything else as a separator.
func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// matchCase applies the case pattern of original to replacement.
// All-upper → all-upper, title-case → title-case, else replacement as-is.
func matchCase(original, replacement string) string {
	if len(original) == 0 || len(replacement) == 0 {
		return replacement
	}

	allUpper := true
	for _, r := range original {
		if unicode.IsLetter(r) && !unicode.IsUpper(r) {
			allUpper = false
			break
		}
	}
	if allUpper {
		return strings.ToUpper(replacement)
	}

	// Title case: first letter upper, rest has at least one lower.
	runes := []rune(original)
	if unicode.IsUpper(runes[0]) {
		rr := []rune(replacement)
		rr[0] = unicode.ToUpper(rr[0])
		return string(rr)
	}

	return replacement
}

// ReplacingTTS wraps a TTS provider and applies word replacements to text
// before synthesis.
type ReplacingTTS struct {
	Inner        TTS
	Replacements map[string]string
}

func (r *ReplacingTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return r.Inner.Synthesize(ctx, ApplyReplacements(text, r.Replacements))
}

// ReplacingSTT wraps an STT provider and applies word replacements to
// transcribed text after transcription.
type ReplacingSTT struct {
	Inner        STT
	Replacements map[string]string
}

func (r *ReplacingSTT) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	text, err := r.Inner.Transcribe(ctx, audioData, filename)
	if err != nil {
		return text, err
	}
	return ApplyReplacements(text, r.Replacements), nil
}

// MergeReplacements merges multiple replacement maps. Later maps take
// precedence over earlier ones (per-agent overrides entry-level, etc.).
func MergeReplacements(maps ...map[string]string) map[string]string {
	total := 0
	for _, m := range maps {
		total += len(m)
	}
	if total == 0 {
		return nil
	}
	merged := make(map[string]string, total)
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged
}

// WrapTTS wraps a TTS provider with word replacements if any are configured.
// Returns the original provider if replacements is empty.
func WrapTTS(t TTS, replacements map[string]string) TTS {
	if len(replacements) == 0 || t == nil {
		return t
	}
	return &ReplacingTTS{Inner: t, Replacements: replacements}
}

// WrapSTT wraps an STT provider with word replacements if any are configured.
// Returns the original provider if replacements is empty.
func WrapSTT(s STT, replacements map[string]string) STT {
	if len(replacements) == 0 || s == nil {
		return s
	}
	return &ReplacingSTT{Inner: s, Replacements: replacements}
}
