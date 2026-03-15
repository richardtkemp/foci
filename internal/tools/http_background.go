package tools

import (
	"context"
	"fmt"
	"time"
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

	// Determine effective threshold: explicit background = 0 (always async),
	// auto-background = configured threshold.
	thresholdSecs := 0
	if explicitBackground && notifier != nil {
		// thresholdSecs stays 0 — always background immediately.
	} else if autoBackgroundSecs > 0 && notifier != nil {
		thresholdSecs = autoBackgroundSecs
	} else {
		// No background mode — caller should execute directly.
		return ToolResult{}, nil, false
	}

	bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)

	var hr struct {
		output ToolResult
		err    error
	}
	signal := make(chan struct{})
	go func() {
		hr.output, hr.err = doAndProcess(bgCtx)
		close(signal)
	}()

	formatHTTPNotify := func() string {
		if hr.err != nil {
			return fmt.Sprintf("[HTTP RESULT] Request failed:\n%s\n\nError: %s", displayURL, hr.err)
		}
		return fmt.Sprintf("[HTTP RESULT] Request completed:\n%s\n\n%s", displayURL, hr.output.Text)
	}

	pendingMsg := fmt.Sprintf("Request still running (exceeded %ds threshold). Results will be delivered when complete.\n%s", autoBackgroundSecs, displayURL)
	if explicitBackground {
		pendingMsg = fmt.Sprintf("Request running in background. Results will be delivered when complete.\n%s", displayURL)
	}

	result, err := RunInBackground(ctx, BackgroundParams{
		SessionKey:    sk,
		Notifier:      notifier,
		ThresholdSecs: thresholdSecs,
		Done:          signal,
		SyncResult: func() (ToolResult, error) {
			bgCancel()
			if hr.err != nil {
				return ToolResult{}, hr.err
			}
			return hr.output, nil
		},
		NotifyMessage:  formatHTTPNotify,
		Cleanup:        func() { bgCancel() },
		PendingResult:  TextResult(pendingMsg),
		NotifyOnCancel: false,
	})
	return result, err, true
}
