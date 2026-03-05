package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTurnCallbacksRoundTrip(t *testing.T) {
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
	got := TurnCallbacksFromContext(context.Background())
	if got != nil {
		t.Errorf("expected nil from empty context, got %v", got)
	}
}

func TestSendIntermediateCtxNilSafe(t *testing.T) {
	// Should not panic with no callbacks
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
	// Should not panic with no callbacks
	notifyToolCallCtx(context.Background(), "test", json.RawMessage(`{}`))
}

func TestSignalActivityCtxNilSafe(t *testing.T) {
	// Should not panic with no callbacks
	signalActivityCtx(context.Background())
}

func TestNotifyThinkingCtxNilSafe(t *testing.T) {
	// Should not panic with no callbacks
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

func TestSendVoiceCtxNilSafe(t *testing.T) {
	// Should not panic with no callbacks
	sendVoiceCtx(context.Background(), []byte("data"))

	// Should not deliver empty data
	var called bool
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		VoiceReplyFunc: func(data []byte) { called = true },
	})
	sendVoiceCtx(ctx, nil)
	if called {
		t.Error("should not call VoiceReplyFunc with nil data")
	}
	sendVoiceCtx(ctx, []byte{})
	if called {
		t.Error("should not call VoiceReplyFunc with empty data")
	}
}
