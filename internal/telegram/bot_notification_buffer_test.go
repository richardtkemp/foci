package telegram

import (
	"testing"

	"foci/internal/command"
)

func TestSendNotification_BufferedDuringActiveTurn(t *testing.T) {
	// Proves that SendNotification buffers messages when an agent turn is
	// active (turnCancel non-nil), and drainPendingNotifications sends them.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	// Simulate an active turn by setting turnActive.
	b.turnActive.Store(true)

	b.SendNotification("alert 1")
	b.SendNotification("alert 2")

	if mock.sentCount() != 0 {
		t.Fatalf("expected 0 sends during active turn, got %d", mock.sentCount())
	}

	b.pendingNotifsMu.Lock()
	buffered := len(b.pendingNotifs)
	b.pendingNotifsMu.Unlock()
	if buffered != 2 {
		t.Fatalf("expected 2 buffered notifications, got %d", buffered)
	}

	// Clear turnActive (simulating turn end) and drain.
	b.turnActive.Store(false)

	b.drainPendingNotifications()

	if mock.sentCount() != 2 {
		t.Errorf("expected 2 sends after drain, got %d", mock.sentCount())
	}

	// Buffer should be empty after drain.
	b.pendingNotifsMu.Lock()
	remaining := len(b.pendingNotifs)
	b.pendingNotifsMu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 buffered after drain, got %d", remaining)
	}
}

func TestSendNotification_ImmediateWhenNoTurn(t *testing.T) {
	// Proves that SendNotification sends immediately when no agent turn is active.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	b.SendNotification("immediate alert")

	if mock.sentCount() != 1 {
		t.Errorf("expected 1 immediate send, got %d", mock.sentCount())
	}

	b.pendingNotifsMu.Lock()
	buffered := len(b.pendingNotifs)
	b.pendingNotifsMu.Unlock()
	if buffered != 0 {
		t.Errorf("expected 0 buffered notifications, got %d", buffered)
	}
}

func TestDrainPendingNotifications_Empty(t *testing.T) {
	// Proves that drainPendingNotifications is a no-op when buffer is empty.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	b.drainPendingNotifications()

	if mock.sentCount() != 0 {
		t.Errorf("expected 0 sends for empty drain, got %d", mock.sentCount())
	}
}
