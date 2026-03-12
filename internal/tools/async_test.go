package tools

import "testing"

func TestAsyncNotifierDelivers(t *testing.T) {
	t.Parallel()
	var gotKey, gotMsg, gotReplyTo string
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		gotKey = sk
		gotMsg = msg
		gotReplyTo = replyTo
	})
	n.InjectToAgent("sess-1", "hello", "", "async_notify")
	if gotKey != "sess-1" {
		t.Errorf("key = %q, want %q", gotKey, "sess-1")
	}
	if gotMsg != "hello" {
		t.Errorf("msg = %q, want %q", gotMsg, "hello")
	}
	if gotReplyTo != "" {
		t.Errorf("replyTo = %q, want %q", gotReplyTo, "")
	}
}

func TestAsyncNotifierNilReceiver(t *testing.T) {
	t.Parallel()
	var n *AsyncNotifier
	n.InjectToAgent("sess", "should not panic", "", "") // must not panic
}

func TestAsyncNotifierNilFunc(t *testing.T) {
	t.Parallel()
	n := &AsyncNotifier{}
	n.InjectToAgent("sess", "should not panic", "", "") // must not panic
}

func TestAsyncNotifierPendingCounter(t *testing.T) {
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

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
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

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
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	// MarkDone without MarkPending should not panic or go negative
	n.MarkDone("sess-1")
	if n.HasPending("sess-1") {
		t.Error("should not have pending")
	}
}

func TestAsyncNotifierNilPending(t *testing.T) {
	t.Parallel()
	var n *AsyncNotifier
	// All methods should be safe on nil receiver
	n.MarkPending("sess")
	n.MarkDone("sess")
	if n.HasPending("sess") {
		t.Error("nil receiver should return false")
	}
}
