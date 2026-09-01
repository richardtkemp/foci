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
type FallbackTTS struct {
	Chain []ChainEntry
}

// Synthesize tries each provider in order. The returned error joins every
// attempt's failure, so errors.As still finds a *ratelimit.Error from any link
// and callers keep classifying a quota exhaustion as the routine event it is.
func (f *FallbackTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if len(f.Chain) == 0 {
		return nil, errors.New("no TTS provider configured")
	}
	var errs []error
	for i, e := range f.Chain {
		data, err := e.TTS.Synthesize(ctx, text)
		if err == nil {
			if i > 0 {
				voiceLog.Infof("tts: synthesized via fallback %q (after %d failed provider(s))", e.ID, i)
			}
			return data, nil
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
