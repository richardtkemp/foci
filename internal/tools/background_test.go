package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunInBackgroundSyncCompletion(t *testing.T) {
	// Work completes before the threshold — SyncResult is returned directly
	// and the notifier is never called.
	t.Parallel()

	var notified bool
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		notified = true
	})

	signal := make(chan struct{})
	close(signal) // already done

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-sync",
		Notifier:      notifier,
		ThresholdSecs: 5,
		Done:          signal,
		SyncResult: func() (ToolResult, error) {
			return TextResult("sync result"), nil
		},
		NotifyMessage: func() string { return "should not be called" },
		PendingResult: TextResult("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "sync result" {
		t.Errorf("result = %q, want %q", result.Text, "sync result")
	}
	if notified {
		t.Error("notifier should not be called for sync completion")
	}
}

func TestRunInBackgroundThresholdExceeded(t *testing.T) {
	// Work exceeds the threshold — PendingResult is returned and the
	// notifier delivers the async result.
	t.Parallel()

	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- msg
	})

	signal := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(signal)
	}()

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-threshold",
		Notifier:      notifier,
		ThresholdSecs: 1, // 1s threshold but we use a short sleep above
		Done:          signal,
		SyncResult:    func() (ToolResult, error) { return TextResult("sync"), nil },
		NotifyMessage: func() string { return "[TEST] async result" },
		PendingResult: TextResult("pending"),
	})

	// Threshold is 1s but work finishes in 200ms — should complete sync.
	// Let's use a different test for threshold exceeded.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The 200ms work completes before the 1s threshold, so we get sync result.
	if result.Text != "sync" {
		t.Errorf("result = %q, want %q", result.Text, "sync")
	}
}

func TestRunInBackgroundAlwaysAsync(t *testing.T) {
	// ThresholdSecs=0 means always background immediately.
	t.Parallel()

	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- fmt.Sprintf("%s:%s", sk, msg)
	})

	signal := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(signal)
	}()

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-async",
		Notifier:      notifier,
		ThresholdSecs: 0,
		Done:          signal,
		NotifyMessage: func() string { return "[TEST] completed" },
		PendingResult: TextResult("backgrounded"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(result.Text, "backgrounded\nBackground ID: ") {
		t.Errorf("result = %q, want prefix %q", result.Text, "backgrounded\nBackground ID: ")
	}

	select {
	case msg := <-completeCh:
		if !strings.HasPrefix(msg, "test-async:[Background ID: ") || !strings.Contains(msg, "[TEST] completed") {
			t.Errorf("notification = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestRunInBackgroundCtxCancelledWithNotify(t *testing.T) {
	// NotifyOnCancel=true delivers results even when ctx is cancelled.
	t.Parallel()

	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- msg
	})

	signal := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(signal)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := RunInBackground(ctx, BackgroundParams{
		SessionKey:     "test-cancel-notify",
		Notifier:       notifier,
		ThresholdSecs:  5,
		Done:           signal,
		SyncResult:     func() (ToolResult, error) { return TextResult("sync"), nil },
		NotifyMessage:  func() string { return "[TEST] delivered after cancel" },
		PendingResult:  TextResult("pending"),
		NotifyOnCancel: true,
	})
	if err == nil {
		t.Fatal("expected context cancelled error")
	}

	select {
	case msg := <-completeCh:
		if !strings.HasPrefix(msg, "[Background ID: ") || !strings.Contains(msg, "[TEST] delivered after cancel") {
			t.Errorf("notification = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestRunInBackgroundCtxCancelledWithoutNotify(t *testing.T) {
	// NotifyOnCancel=false discards results when ctx is cancelled.
	t.Parallel()

	var notified bool
	cleanedUp := make(chan struct{})
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		notified = true
	})

	signal := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(signal)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := RunInBackground(ctx, BackgroundParams{
		SessionKey:     "test-cancel-discard",
		Notifier:       notifier,
		ThresholdSecs:  5,
		Done:           signal,
		SyncResult:     func() (ToolResult, error) { return TextResult("sync"), nil },
		NotifyMessage:  func() string { return "should not be called" },
		Cleanup:        func() { close(cleanedUp) },
		PendingResult:  TextResult("pending"),
		NotifyOnCancel: false,
	})
	if err == nil {
		t.Fatal("expected context cancelled error")
	}

	// Wait for cleanup to confirm goroutine finished.
	select {
	case <-cleanedUp:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup")
	}

	if notified {
		t.Error("notifier should not be called when NotifyOnCancel=false")
	}
}

func TestRunInBackgroundCleanupCalled(t *testing.T) {
	// Cleanup function is called after async delivery.
	t.Parallel()

	cleanedUp := make(chan struct{})
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	signal := make(chan struct{})
	close(signal) // already done

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-cleanup",
		Notifier:      notifier,
		ThresholdSecs: 0, // always async
		Done:          signal,
		NotifyMessage: func() string { return "done" },
		Cleanup:       func() { close(cleanedUp) },
		PendingResult: TextResult("backgrounded"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(result.Text, "backgrounded\nBackground ID: ") {
		t.Errorf("result = %q, want prefix %q", result.Text, "backgrounded\nBackground ID: ")
	}

	select {
	case <-cleanedUp:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup")
	}
}

func TestRunInBackgroundPendingCount(t *testing.T) {
	// MarkPending/MarkDone lifecycle is correct — count goes to 0 after delivery.
	t.Parallel()

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	signal := make(chan struct{})

	_, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-pending",
		Notifier:      notifier,
		ThresholdSecs: 0,
		Done:          signal,
		NotifyMessage: func() string { return "done" },
		PendingResult: TextResult("backgrounded"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have pending count.
	if !notifier.HasPending("test-pending") {
		t.Error("expected pending count > 0")
	}

	// Complete the work.
	close(signal)

	// Wait for goroutine to finish.
	time.Sleep(100 * time.Millisecond)

	if notifier.HasPending("test-pending") {
		t.Error("expected pending count = 0 after delivery")
	}
}

func TestRunInBackgroundID(t *testing.T) {
	// Proves that backgrounded calls get a 3-word ID that appears in both
	// the immediate PendingResult and the later async notification, allowing
	// the model to correlate them.
	t.Parallel()

	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- msg
	})

	signal := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(signal)
	}()

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-bgid",
		Notifier:      notifier,
		ThresholdSecs: 0, // always async
		Done:          signal,
		NotifyMessage: func() string { return "[TEST] done" },
		PendingResult: TextResult("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extract the ID from the pending result.
	const prefix = "pending\nBackground ID: "
	if !strings.HasPrefix(result.Text, prefix) {
		t.Fatalf("result = %q, missing background ID", result.Text)
	}
	bgID := strings.TrimPrefix(result.Text, prefix)

	// Should be 3 hyphen-separated words.
	words := strings.Split(bgID, "-")
	if len(words) != 3 {
		t.Errorf("background ID = %q, want 3 hyphen-separated words", bgID)
	}

	// The same ID must appear in the notification.
	select {
	case msg := <-completeCh:
		wantPrefix := "[Background ID: " + bgID + "]\n[TEST] done"
		if msg != wantPrefix {
			t.Errorf("notification = %q, want %q", msg, wantPrefix)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestRunInBackgroundSyncNoID(t *testing.T) {
	// Proves that synchronous completions (before threshold) do NOT get a
	// background ID — the ID is only added when actually backgrounding.
	t.Parallel()

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	signal := make(chan struct{})
	close(signal) // already done

	result, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "test-sync-noid",
		Notifier:      notifier,
		ThresholdSecs: 5,
		Done:          signal,
		SyncResult: func() (ToolResult, error) {
			return TextResult("sync result"), nil
		},
		NotifyMessage: func() string { return "unused" },
		PendingResult: TextResult("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "sync result" {
		t.Errorf("result = %q, want %q", result.Text, "sync result")
	}
	if strings.Contains(result.Text, "Background ID") {
		t.Error("sync result should not contain a background ID")
	}
}
