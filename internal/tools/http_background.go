package tools

import (
	"context"
	"fmt"
	"time"

	"foci/internal/log"
)

// runHTTPBackground handles both explicit and auto-background HTTP execution modes.
// Returns (result, err, handled). When handled is true, the caller should return
// the result directly. When false, the caller should fall through to direct execution.
func runHTTPBackground(
	ctx context.Context,
	doAndProcess func(context.Context) (ToolResult, error),
	displayURL string,
	timeout time.Duration,
	autoBackgroundSecs int,
	explicitBackground bool,
	notifier *AsyncNotifier,
) (ToolResult, error, bool) {
	sk := SessionKeyFromContext(ctx)

	// Explicit background mode: fire immediately
	if explicitBackground && notifier != nil {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)
		notifier.MarkPending(sk)
		go func() {
			defer bgCancel()
			defer notifier.MarkDone(sk)
			result, err := doAndProcess(bgCtx)
			var msg string
			if err != nil {
				msg = fmt.Sprintf("[HTTP RESULT] Request failed:\n%s\n\nError: %s", displayURL, err)
			} else {
				msg = fmt.Sprintf("[HTTP RESULT] Request completed:\n%s\n\n%s", displayURL, result.Text)
			}
			notifier.Notify(sk, msg)
		}()
		return TextResult(fmt.Sprintf("Request running in background. Results will be delivered when complete.\n%s", displayURL)), nil, true
	}

	// Auto-background: start the request and wait with a timer
	if autoBackgroundSecs > 0 && notifier != nil {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)

		type httpResult struct {
			output ToolResult
			err    error
		}
		done := make(chan httpResult, 1)
		go func() {
			out, err := doAndProcess(bgCtx)
			done <- httpResult{out, err}
		}()

		threshold := time.Duration(autoBackgroundSecs) * time.Second
		select {
		case r := <-done:
			bgCancel()
			if r.err != nil {
				return ToolResult{}, r.err, true
			}
			return r.output, nil, true

		case <-time.After(threshold):
			log.Infof("http_request", "auto-backgrounding after %v: %s", threshold, displayURL)
			notifier.MarkPending(sk)
			go func() {
				defer bgCancel()
				defer notifier.MarkDone(sk)
				r := <-done
				var msg string
				if r.err != nil {
					msg = fmt.Sprintf("[HTTP RESULT] Request failed:\n%s\n\nError: %s", displayURL, r.err)
				} else {
					msg = fmt.Sprintf("[HTTP RESULT] Request completed:\n%s\n\n%s", displayURL, r.output.Text)
				}
				notifier.Notify(sk, msg)
			}()
			return TextResult(fmt.Sprintf("Request still running (exceeded %ds threshold). Results will be delivered when complete.\n%s", autoBackgroundSecs, displayURL)), nil, true

		case <-ctx.Done():
			// Agent turn cancelled — let the request continue in background
			go func() {
				defer bgCancel()
				<-done
			}()
			return ToolResult{}, ctx.Err(), true
		}
	}

	// No background mode — caller should execute directly
	return ToolResult{}, nil, false
}
