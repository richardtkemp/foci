package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ChainEntry pairs a resolved TTS provider with the config id it came from, so
// a failure can name which provider produced it.
type ChainEntry struct {
	ID  string
	TTS TTS
}

// FallbackTTS synthesizes through the first provider in Chain that succeeds.
//
// There is deliberately no retry, backoff or wait anywhere in this path. A
// spoken reply is live: by the time a rate-limit window expires the moment it
// belonged to has passed, so waiting it out spends quota on audio nobody is
// still listening for. The only useful response to a failed provider is an
// immediate attempt against a different one — and if none is left, text-only.
// A text over a link's max_chars skips that link in the same way (#2919).
type FallbackTTS struct {
	Chain []ChainEntry
}

// Synthesize tries each provider in order. The returned error joins every
// attempt's failure, so errors.As still finds a *ratelimit.Error from any link
// and callers keep classifying a quota exhaustion as the routine event it is.
// When every link refused the text as too long, the error is one
// *TextTooLongError. A skipped link in a chain that also failed for another
// reason is text only, so errors.As cannot report the whole chain as a skip.
func (f *FallbackTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if len(f.Chain) == 0 {
		return nil, errors.New("no TTS provider configured")
	}
	var errs []error
	var tooLong *TextTooLongError
	skipped := 0
	for i, e := range f.Chain {
		data, err := e.TTS.Synthesize(ctx, text)
		if err == nil {
			if i > 0 {
				voiceLog.Infof("tts: synthesized via fallback %q (after %d provider(s) failed or skipped)", e.ID, i)
			}
			return data, nil
		}
		var skip *TextTooLongError
		if errors.As(err, &skip) {
			tooLong = skip
			skipped++
			errs = append(errs, fmt.Errorf("%s: %v", e.ID, err)) //nolint:errorlint // %v on purpose: see the doc comment
			if i+1 < len(f.Chain) {
				voiceLog.Infof("tts %q skipped (%v) — falling back to %q", e.ID, err, f.Chain[i+1].ID)
			}
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %w", e.ID, err))
		// A dead context fails every remaining provider identically — stop
		// rather than spend the rest of the chain rediscovering that.
		if ctx.Err() != nil {
			break
		}
		if i+1 < len(f.Chain) {
			voiceLog.Infof("tts %q failed (%v) — falling back to %q", e.ID, err, f.Chain[i+1].ID)
		}
	}
	if skipped == len(errs) {
		return nil, tooLong
	}
	return nil, newChainError(errs)
}

// chainError renders every link's failure on ONE line while keeping each
// wrapped error reachable through errors.As/Is. errors.Join alone would do the
// unwrapping but renders one failure per line, and a multi-line log entry is
// exactly the noise the ratelimit package exists to keep out of the log.
type chainError struct {
	joined error
	msg    string
}

func newChainError(errs []error) error {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return &chainError{joined: errors.Join(errs...), msg: strings.Join(msgs, "; ")}
}

func (c *chainError) Error() string { return c.msg }
func (c *chainError) Unwrap() error { return c.joined }
