package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTurnCallbacksRoundTrip(t *testing.T) {
	// Proves that TurnCallbacks stored in a context via WithTurnCallbacks can be retrieved with TurnCallbacksFromContext and that the retrieved callbacks are the exact same object.
	var called bool
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) { called = true },
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	got := TurnCallbacksFromContext(ctx)
	if got == nil {
		t.Fatal("expected non-nil callbacks from context")
	}
	if got != cb {
		t.Error("round-tripped callbacks don't match")
	}
	got.ReplyFunc("test")
	if !called {
		t.Error("ReplyFunc was not called")
	}
}

func TestTurnCallbacksNilContext(t *testing.T) {
	// Proves that TurnCallbacksFromContext returns nil when no callbacks have been stored in the context.
	got := TurnCallbacksFromContext(context.Background())
	if got != nil {
		t.Errorf("expected nil from empty context, got %v", got)
	}
}

func TestSendIntermediateCtxNilSafe(t *testing.T) {
	// Proves that sendIntermediateCtx is nil-safe: it does not panic when the context has no callbacks, when ReplyFunc is nil, or when called with empty text.
	sendIntermediateCtx(context.Background(), "test")

	// Should not panic with nil ReplyFunc
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{})
	sendIntermediateCtx(ctx, "test")

	// Should not call with empty text
	var called bool
	ctx = WithTurnCallbacks(context.Background(), &TurnCallbacks{
		ReplyFunc: func(text string) { called = true },
	})
	sendIntermediateCtx(ctx, "")
	if called {
		t.Error("should not call ReplyFunc with empty text")
	}
}

func TestNotifyToolCallCtxNilSafe(t *testing.T) {
	// Proves that notifyToolCallCtx does not panic when the context has no callbacks registered.
	notifyToolCallCtx(context.Background(), "test", json.RawMessage(`{}`))
}

func TestSignalActivityCtxNilSafe(t *testing.T) {
	// Proves that signalActivityCtx does not panic when the context has no callbacks registered.
	signalActivityCtx(context.Background())
}

func TestNotifyThinkingCtxNilSafe(t *testing.T) {
	// Proves that notifyThinkingCtx is nil-safe and guards against empty input: it does not panic without callbacks, skips empty thinking text, and correctly delivers non-empty thinking to the observer.
	notifyThinkingCtx(context.Background(), "some thinking")

	// Should not panic with nil ThinkingObserver
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{})
	notifyThinkingCtx(ctx, "some thinking")

	// Should not call with empty thinking
	var called bool
	ctx = WithTurnCallbacks(context.Background(), &TurnCallbacks{
		ThinkingObserver: func(thinking string) { called = true },
	})
	notifyThinkingCtx(ctx, "")
	if called {
		t.Error("should not call ThinkingObserver with empty thinking")
	}

	// Should call with non-empty thinking
	var got string
	ctx = WithTurnCallbacks(context.Background(), &TurnCallbacks{
		ThinkingObserver: func(thinking string) { got = thinking },
	})
	notifyThinkingCtx(ctx, "internal reasoning")
	if got != "internal reasoning" {
		t.Errorf("ThinkingObserver got %q, want %q", got, "internal reasoning")
	}
}

