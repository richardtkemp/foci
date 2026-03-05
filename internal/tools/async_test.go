package tools

import "testing"

func TestAsyncNotifierDelivers(t *testing.T) {
	var gotKey, gotMsg string
	n := NewAsyncNotifier(func(sk, msg string) {
		gotKey = sk
		gotMsg = msg
	})
	n.Notify("sess-1", "hello")
	if gotKey != "sess-1" {
		t.Errorf("key = %q, want %q", gotKey, "sess-1")
	}
	if gotMsg != "hello" {
		t.Errorf("msg = %q, want %q", gotMsg, "hello")
	}
}

func TestAsyncNotifierNilReceiver(t *testing.T) {
	var n *AsyncNotifier
	n.Notify("sess", "should not panic") // must not panic
}

func TestAsyncNotifierNilFunc(t *testing.T) {
	n := &AsyncNotifier{}
	n.Notify("sess", "should not panic") // must not panic
}

func TestAsyncNotifierPendingCounter(t *testing.T) {
	n := NewAsyncNotifier(func(sk, msg string) {})

	if n.HasPending("sess-1") {
		t.Error("should not have pending before MarkPending")
	}

	n.MarkPending("sess-1")
	if !n.HasPending("sess-1") {
		t.Error("should have pending after MarkPending")
	}

	// Different session should not be affected
	if n.HasPending("sess-2") {
		t.Error("sess-2 should not have pending")
	}

	n.MarkDone("sess-1")
	if n.HasPending("sess-1") {
		t.Error("should not have pending after MarkDone")
	}
}

func TestAsyncNotifierMultiplePending(t *testing.T) {
	n := NewAsyncNotifier(func(sk, msg string) {})

	n.MarkPending("sess-1")
	n.MarkPending("sess-1")
	n.MarkPending("sess-1")

	n.MarkDone("sess-1")
	if !n.HasPending("sess-1") {
		t.Error("should still have pending (2 remaining)")
	}

	n.MarkDone("sess-1")
	n.MarkDone("sess-1")
	if n.HasPending("sess-1") {
		t.Error("should not have pending after all MarkDone")
	}
}

func TestAsyncNotifierMarkDoneUnderflow(t *testing.T) {
	n := NewAsyncNotifier(func(sk, msg string) {})

	// MarkDone without MarkPending should not panic or go negative
	n.MarkDone("sess-1")
	if n.HasPending("sess-1") {
		t.Error("should not have pending")
	}
}

func TestAsyncNotifierNilPending(t *testing.T) {
	var n *AsyncNotifier
	// All methods should be safe on nil receiver
	n.MarkPending("sess")
	n.MarkDone("sess")
	if n.HasPending("sess") {
		t.Error("nil receiver should return false")
	}
}
