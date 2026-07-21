package ratelimit

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mode selects what a Transport does when it hits a rate limit it will not (or
// cannot) wait out within its inline budget.
type Mode int

const (
	// ModeDegrade returns a typed *Error instead of a response, so the caller
	// degrades gracefully (e.g. voice-mode falls back to text-only). The 429
	// body is discarded — the caller reports a clean, body-free message.
	ModeDegrade Mode = iota
	// ModePassthrough returns the final 429/503 response unchanged (body intact),
	// so the caller inspects the real status and body itself (e.g. the agent's
	// http_request tool, which surfaces the response to the model).
	ModePassthrough
)

// maxHintBytes bounds how much of a rate-limit response body is read to look for
// a "try again in …" hint. Rate-limit bodies are small; this is a safety cap.
const maxHintBytes = 8 << 10

// retryHintRE matches the compact "try again in 7m12s" / "try again in 20s"
// phrasing OpenAI-compatible APIs (Groq, OpenAI) put in a 429 body. The captured
// token is fed to time.ParseDuration.
var retryHintRE = regexp.MustCompile(`(?i)try again in ([0-9hms.]+)`)

// parseRetryHint extracts a wait duration from a rate-limit body, or 0 if none.
func parseRetryHint(body string) time.Duration {
	m := retryHintRE.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimRight(m[1], "."))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// Error is returned by a Transport in ModeDegrade when a request is rate-limited
// and was not retried within the inline budget. It carries the resolved reset
// deadline so callers can report a clean, body-free message and (if they wish)
// gate future calls. The 429 body is deliberately NOT part of Error() — dumping
// it is exactly the noise (embedded URLs, JSON) this package exists to avoid.
type Error struct {
	StatusCode int
	Until      time.Time     // absolute reset deadline (from Resolve)
	RetryAfter time.Duration // best-effort wait remaining when the error was built
	Detail     string        // raw body snippet, for debug logging only — never user-facing
}

func (e *Error) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (%d): retry in %s", e.StatusCode, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("rate limited (%d)", e.StatusCode)
}

// Transport is an http.RoundTripper that recognises 429/503 responses, resolves
// a reset deadline from their Retry-After header or body hint via the shared
// policy, and either retries within a bounded inline budget or hands control back
// to the caller per Mode. It is stateless and safe for concurrent use.
type Transport struct {
	Base          http.RoundTripper // nil => http.DefaultTransport
	Kind          Kind              // request vs usage window (only matters when no hint is present)
	Mode          Mode              // give-up behaviour
	MaxInlineWait time.Duration     // longest a single retry may block; 0 => never retry (degrade/passthrough immediately)
	MaxRetries    int               // max retries when MaxInlineWait > 0
	Now           func() time.Time  // nil => time.Now (injectable for tests)
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func isLimited(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	// A body can only be replayed for a retry if it's absent or reconstructable.
	replayable := req.Body == nil || req.GetBody != nil

	for attempt := 0; ; attempt++ {
		attemptReq := req
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attemptReq = req.Clone(req.Context())
			attemptReq.Body = body
		}

		resp, err := base.RoundTrip(attemptReq)
		if err != nil || !isLimited(resp.StatusCode) {
			return resp, err
		}

		sig, restore := t.readSignal(resp)
		res := Resolve(t.now(), sig, 0)
		wait := res.Until.Sub(t.now())

		canRetry := t.MaxInlineWait > 0 && attempt < t.MaxRetries && replayable &&
			wait > 0 && wait <= t.MaxInlineWait
		if canRetry {
			_ = resp.Body.Close() // discard the 429 body; we're retrying
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			continue
		}

		// Give up: hand control back per Mode.
		if t.Mode == ModePassthrough {
			restore() // put the peeked bytes back so the caller reads the full body
			return resp, nil
		}
		_ = resp.Body.Close()
		return nil, &Error{StatusCode: resp.StatusCode, Until: res.Until, RetryAfter: wait, Detail: sig.Detail}
	}
}

// readSignal parses a rate-limit Signal from resp's Retry-After header and body
// hint. It returns a restore closure that, if called, prepends the peeked body
// bytes back onto resp.Body — used only in ModePassthrough, where the caller must
// still read the untouched body.
func (t *Transport) readSignal(resp *http.Response) (Signal, func()) {
	sig := Signal{Kind: t.Kind}
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			sig.RetryAfter = time.Duration(secs) * time.Second
		} else if ts, err := http.ParseTime(ra); err == nil {
			sig.ResetAt = ts
		}
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, maxHintBytes))
	sig.Detail = strings.TrimSpace(string(peek))
	if sig.RetryAfter == 0 && sig.ResetAt.IsZero() {
		if d := parseRetryHint(sig.Detail); d > 0 {
			sig.RetryAfter = d
		}
	}
	orig := resp.Body
	restore := func() {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), orig), orig}
	}
	return sig, restore
}
