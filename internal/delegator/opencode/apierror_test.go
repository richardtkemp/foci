package opencode

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

func TestHandleMessageError_APIError_LogsStructuredDetails(t *testing.T) {
	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}

	data, _ := json.Marshal(ApiErrorData{
		Message:     "Quota exceeded. Check your plan and billing details.",
		StatusCode:  429,
		IsRetryable: false,
	})

	// Should not panic, should not fail the turn (message error ≠ session error)
	b.handleMessageError(&MessageError{
		Name: ErrAPI,
		Data: data,
	})
}

func TestOnSessionError_APIError_InjectsMessageIntoTurnText(t *testing.T) {
	var completed *delegator.TurnResult

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})

	data, _ := json.Marshal(ApiErrorData{
		Message:     "Quota exceeded. Check your plan and billing details.",
		StatusCode:  429,
		IsRetryable: false,
	})

	b.onSessionError("sess-test", &MessageError{
		Name: ErrAPI,
		Data: data,
	})

	if completed == nil {
		t.Fatal("expected OnTurnComplete to fire")
	}
	if completed.Text != "⚠️ Quota exceeded. Check your plan and billing details." {
		t.Errorf("turn text = %q, want quota message", completed.Text)
	}
}

func TestOnSessionError_APIError_DoesNotClobberExistingText(t *testing.T) {
	var completed *delegator.TurnResult

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})

	// Simulate partial output already streamed
	b.turnMu.Lock()
	b.turnText.WriteString("Here are the results so far...")
	b.turnMu.Unlock()

	data, _ := json.Marshal(ApiErrorData{
		Message:    "Server error.",
		StatusCode: 503,
	})
	b.onSessionError("sess-test", &MessageError{
		Name: ErrAPI,
		Data: data,
	})

	if completed == nil {
		t.Fatal("expected OnTurnComplete to fire")
	}
	// Should deliver the partial text, NOT the API error message
	if completed.Text != "Here are the results so far..." {
		t.Errorf("turn text = %q, want existing partial text", completed.Text)
	}
}

func TestOnSessionError_APIError_RetryableSurfacesCorrectly(t *testing.T) {
	var completed *delegator.TurnResult

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})

	data, _ := json.Marshal(ApiErrorData{
		Message:     "Server is overloaded. Please retry.",
		StatusCode:  529,
		IsRetryable: true,
	})
	b.onSessionError("sess-test", &MessageError{
		Name: ErrAPI,
		Data: data,
	})

	if completed == nil {
		t.Fatal("expected OnTurnComplete to fire")
	}
	if completed.Text != "⚠️ Server is overloaded. Please retry." {
		t.Errorf("turn text = %q, want overload message", completed.Text)
	}
}

func TestOnSessionError_APIError_UnparsableData_FallsBackGracefully(t *testing.T) {
	var completed *delegator.TurnResult

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})

	// Garbage data — should not panic, should still fail turn
	b.onSessionError("sess-test", &MessageError{
		Name: ErrAPI,
		Data: json.RawMessage(`{not valid json`),
	})

	if completed == nil {
		t.Fatal("expected OnTurnComplete to fire even with unparsable data")
	}
	// Falls back to the generic failInFlightTurn message
	if completed.Text == "" {
		t.Error("expected non-empty fallback text")
	}
}
