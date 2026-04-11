package agent

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/agent/turnevent"
)

// TestEmitIntermediateTextEmitsTextBlock proves that emitIntermediateText
// routes to the ctx sink as an intermediate TextBlock (not a delta or final),
// and no-ops for empty text.
func TestEmitIntermediateTextEmitsTextBlock(t *testing.T) {
	var events []turnevent.Event
	ctx := turnevent.WithSink(context.Background(), fnSink(func(_ context.Context, ev turnevent.Event) {
		events = append(events, ev)
	}))

	emitIntermediateText(ctx, "hello")
	emitIntermediateText(ctx, "")

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	tb, ok := events[0].(turnevent.TextBlock)
	if !ok {
		t.Fatalf("event[0] = %T, want TextBlock", events[0])
	}
	if tb.Text != "hello" || tb.Phase != turnevent.PhaseIntermediate {
		t.Errorf("TextBlock = %+v, want {hello,Intermediate}", tb)
	}
}

// TestEmitToolCallNilSafe proves emitToolCall does not panic when ctx has no
// sink attached — the default NopSink fallback must absorb the emit.
func TestEmitToolCallNilSafe(t *testing.T) {
	emitToolCall(context.Background(), "test", "t1", json.RawMessage(`{}`))
}

// TestEmitThinkingBlockSkipsEmpty proves the helper avoids emitting empty
// thinking text so sinks don't receive meaningless ThinkingBlock events.
func TestEmitThinkingBlockSkipsEmpty(t *testing.T) {
	var events []turnevent.Event
	ctx := turnevent.WithSink(context.Background(), fnSink(func(_ context.Context, ev turnevent.Event) {
		events = append(events, ev)
	}))

	emitThinkingBlock(ctx, "")
	if len(events) != 0 {
		t.Errorf("empty thinking emitted: %v", events)
	}

	emitThinkingBlock(ctx, "reasoning")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	tb, ok := events[0].(turnevent.ThinkingBlock)
	if !ok || tb.Text != "reasoning" {
		t.Errorf("event[0] = %v, want ThinkingBlock{reasoning}", events[0])
	}
}

// TestSteerBlocksViaSteerer proves steerBlocks pulls from the ctx-attached
// Steerer and wraps each message in a `[user] ...` content block — this is
// the path the agent uses to drain pending steers between tool calls.
func TestSteerBlocksViaSteerer(t *testing.T) {
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string {
		return []string{"wait", "use bun"}
	}))

	blocks := steerBlocks(ctx)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Text != "[user] wait" || blocks[1].Text != "[user] use bun" {
		t.Errorf("blocks = %+v", blocks)
	}
}

// TestSteerBlocksAbsent proves steerBlocks returns nil when no steerer is set,
// which is the common path for HTTP and hook-driven turns.
func TestSteerBlocksAbsent(t *testing.T) {
	if blocks := steerBlocks(context.Background()); blocks != nil {
		t.Errorf("blocks = %v, want nil", blocks)
	}
}
