package tools

import "testing"

// TestMigrateSession_InjectResolvesToNewKey verifies that after migration,
// InjectToAgent delivers to the new key even when called with the old key.
func TestMigrateSession_InjectResolvesToNewKey(t *testing.T) {
	t.Parallel()
	var delivered string
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered = sk
	})

	n.MarkPending("old/key")
	n.MigrateSession("old/key", "new/key")

	n.InjectToAgent("old/key", "result", "", "")
	if delivered != "new/key" {
		t.Errorf("InjectToAgent delivered to %q, want %q", delivered, "new/key")
	}
}

// TestMigrateSession_MarkDoneResolvesToNewKey verifies that MarkDone
// decrements the new key's count when called with the old key.
func TestMigrateSession_MarkDoneResolvesToNewKey(t *testing.T) {
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	n.MarkPending("old/key")
	n.MarkPending("old/key")
	n.MigrateSession("old/key", "new/key")

	if !n.HasPending("new/key") {
		t.Fatal("new key should have pending after migration")
	}
	if n.HasPending("old/key") {
		t.Fatal("old key should not have pending after migration")
	}

	n.MarkDone("old/key") // should decrement new/key
	if !n.HasPending("new/key") {
		t.Fatal("new key should still have 1 pending")
	}

	n.MarkDone("old/key") // should decrement new/key to 0
	if n.HasPending("new/key") {
		t.Fatal("new key should have no pending after all MarkDone")
	}
}

// TestMigrateSession_PendingCountMerges verifies that migrating into a key
// that already has pending results adds the counts together.
func TestMigrateSession_PendingCountMerges(t *testing.T) {
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	n.MarkPending("old/key")
	n.MarkPending("old/key")
	n.MarkPending("new/key") // pre-existing pending on new key

	n.MigrateSession("old/key", "new/key")

	// 2 from old + 1 pre-existing = 3 total
	n.MarkDone("new/key")
	n.MarkDone("new/key")
	if !n.HasPending("new/key") {
		t.Fatal("should still have 1 pending")
	}
	n.MarkDone("new/key")
	if n.HasPending("new/key") {
		t.Fatal("should have no pending after 3 MarkDone")
	}
}

// TestMigrateSession_ChainedRotation verifies that multiple rotations
// (A→B→C) flatten correctly — old goroutines holding key A resolve to C.
func TestMigrateSession_ChainedRotation(t *testing.T) {
	t.Parallel()
	var delivered string
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered = sk
	})

	n.MarkPending("key/a")
	n.MigrateSession("key/a", "key/b")
	n.MigrateSession("key/b", "key/c")

	// Goroutine still holding key/a should resolve to key/c
	n.InjectToAgent("key/a", "result", "", "")
	if delivered != "key/c" {
		t.Errorf("chained inject delivered to %q, want %q", delivered, "key/c")
	}

	// MarkDone with key/a should decrement key/c
	if !n.HasPending("key/c") {
		t.Fatal("key/c should have pending")
	}
	n.MarkDone("key/a")
	if n.HasPending("key/c") {
		t.Fatal("key/c should have no pending after MarkDone(key/a)")
	}
}

// TestMigrateSession_NoOp verifies that migration is a no-op when old
// equals new, when newKey is empty, or on a nil receiver.
func TestMigrateSession_NoOp(t *testing.T) {
	t.Parallel()
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})

	n.MarkPending("sess")
	n.MigrateSession("sess", "sess") // same key
	if !n.HasPending("sess") {
		t.Fatal("same-key migration should preserve pending")
	}

	n.MigrateSession("sess", "") // empty new key
	if !n.HasPending("sess") {
		t.Fatal("empty-new-key migration should preserve pending")
	}

	// Nil receiver should not panic
	var nilNotifier *AsyncNotifier
	nilNotifier.MigrateSession("a", "b")
}

// TestMigrateSession_NoPendingStillRemaps verifies that migration installs
// a remap even when there are no pending results, so that a slow goroutine
// that calls InjectToAgent after MarkDone still resolves correctly.
func TestMigrateSession_NoPendingStillRemaps(t *testing.T) {
	t.Parallel()
	var delivered string
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered = sk
	})

	// No MarkPending — but migration should still remap
	n.MigrateSession("old/key", "new/key")
	n.InjectToAgent("old/key", "late result", "", "")
	if delivered != "new/key" {
		t.Errorf("delivered to %q, want %q", delivered, "new/key")
	}
}

// TestMigrateSession_ReplyToSessionUnchanged verifies that replyToSession
// is passed through unmodified (only targetSession is remapped).
func TestMigrateSession_ReplyToSessionUnchanged(t *testing.T) {
	t.Parallel()
	var gotTarget, gotReplyTo string
	n := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		gotTarget = sk
		gotReplyTo = replyTo
	})

	n.MigrateSession("old/key", "new/key")
	n.InjectToAgent("old/key", "msg", "reply/sess", "")

	if gotTarget != "new/key" {
		t.Errorf("target = %q, want %q", gotTarget, "new/key")
	}
	if gotReplyTo != "reply/sess" {
		t.Errorf("replyTo = %q, want %q", gotReplyTo, "reply/sess")
	}
}
