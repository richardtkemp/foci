package tools

import (
	"context"
	"time"

	"foci/internal/secrets"
)

// BackgroundParams configures RunInBackground.
type BackgroundParams struct {
	// SessionKey identifies which session receives async results.
	SessionKey string

	// Notifier delivers async results. Must be non-nil.
	Notifier *AsyncNotifier

	// ThresholdSecs is how long to wait before backgrounding.
	// 0 means always background immediately (no threshold wait).
	ThresholdSecs int

	// Done is closed when the spawned work completes.
	Done <-chan struct{}

	// SyncResult is called when work completes before the threshold.
	// Only used when ThresholdSecs > 0.
	SyncResult func() (ToolResult, error)

	// NotifyMessage is called in the background goroutine to build the
	// notification message injected to the agent. Must block until Done
	// has been read (it will already be closed when called).
	NotifyMessage func() string

	// Cleanup is called after async delivery completes (optional).
	Cleanup func()

	// PendingResult is returned to the caller when the work is backgrounded
	// (threshold exceeded path). Ignored on the ctx.Done path.
	PendingResult ToolResult

	// NotifyOnCancel controls behavior when ctx is cancelled before the
	// threshold. If true, results are still delivered via the notifier
	// (shell behavior). If false, the work runs to completion but results
	// are discarded (http behavior).
	NotifyOnCancel bool
}

// RunInBackground handles the threshold-wait-or-background pattern shared by
// exec, http_request, and spawn tools. It waits up to ThresholdSecs for work
// to complete synchronously, then falls back to async delivery via the notifier.
func RunInBackground(ctx context.Context, p BackgroundParams) (ToolResult, error) {
	if p.ThresholdSecs > 0 {
		threshold := time.Duration(p.ThresholdSecs) * time.Second
		select {
		case <-p.Done:
			return p.SyncResult()

		case <-time.After(threshold):
			// Fall through to background delivery.

		case <-ctx.Done():
			if !p.NotifyOnCancel {
				// Fire and forget — run cleanup when done, discard result.
				go func() {
					if p.Cleanup != nil {
						defer p.Cleanup()
					}
					<-p.Done
				}()
				return ToolResult{}, ctx.Err()
			}
			// Fall through to background delivery (same as threshold path).
		}
	}

	// Generate a 3-word ID to correlate the pending result with the later notification.
	if bgID, err := secrets.GeneratePassphrase(3); err == nil && bgID != "" {
		p.PendingResult.Text += "\nBackground ID: " + bgID
		origNotify := p.NotifyMessage
		p.NotifyMessage = func() string {
			return "[Background ID: " + bgID + "]\n" + origNotify()
		}
	}

	// Background: mark pending, deliver result asynchronously.
	p.Notifier.MarkPending(p.SessionKey)
	go func() {
		defer p.Notifier.MarkDone(p.SessionKey)
		if p.Cleanup != nil {
			defer p.Cleanup()
		}
		<-p.Done
		msg := p.NotifyMessage()
		p.Notifier.InjectToAgent(p.SessionKey, msg, "", "async_notify")
	}()

	if ctx.Err() != nil {
		return ToolResult{}, ctx.Err()
	}
	return p.PendingResult, nil
}
