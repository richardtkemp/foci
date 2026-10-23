package app

import (
	"context"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/turnevent"
)

// #2260: Claude Code's api_retry reaches the app as a turnevent.BackendRetry
// and shows as the ephemeral "retrying" activity until the main call goes
// through (the next content or tool event) or the turn ends.

func TestRetryDetail(t *testing.T) {
	for _, tc := range []struct {
		status, attempt int
		delay           time.Duration
		want            string
	}{
		{529, 3, 2 * time.Minute, "API overloaded (529), retry 3, next try in 2m"},
		{429, 1, 2 * time.Second, "Rate limited (429), retry 1, next try in 2s"},
		{500, 2, 4516 * time.Millisecond, "API error 500, retry 2, next try in 5s"},
		{0, 2, 30 * time.Second, "Connection error, retry 2, next try in 30s"},
		{529, 1, 0, "API overloaded (529), retry 1, retrying now"},
		{529, 1, time.Millisecond, "API overloaded (529), retry 1, next try in 1s"},
		{529, 1, 59*time.Second + 500*time.Millisecond, "API overloaded (529), retry 1, next try in 60s"},
		{529, 4, 90 * time.Second, "API overloaded (529), retry 4, next try in 2m"},
		{429, 20, 2 * time.Hour, "Rate limited (429), retry 20, next try in 2h"},
		{429, 20, 5*time.Hour + 47*time.Minute + 10*time.Second, "Rate limited (429), retry 20, next try in 5h 47m"},
	} {
		if got := retryDetail(tc.status, tc.attempt, tc.delay); got != tc.want {
			t.Errorf("retryDetail(%d, %d, %v) = %q, want %q", tc.status, tc.attempt, tc.delay, got, tc.want)
		}
	}
}

// activityFrames is the Activity frames among got.
func activityFrames(got []decoded) []map[string]any {
	var out []map[string]any
	for _, d := range got {
		if d.t == fap.TypeActivity {
			out = append(out, d.d)
		}
	}
	return out
}

var testRetry = turnevent.BackendRetry{Endpoint: "Claude Code", Attempt: 3, MaxRetries: 15, Delay: 2 * time.Minute, Status: 529, Category: "overloaded"}

const testRetryText = "API overloaded (529), retry 3, next try in 2m"

// TestAppSink_RetryingActivity: a retry shows as one "retrying" frame and in
// the roster; a heartbeat and subagent events leave it; each clearing event
// sends exactly one frame, back to the turn-scoped kind it now is, with no
// stale "retrying" after it.
func TestAppSink_RetryingActivity(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		clear func(s *appSink)
		want  string // the kind after clearing
	}{
		{"text delta", func(s *appSink) { s.Emit(ctx, turnevent.TextDelta{Delta: "hi"}) }, "typing"},
		{"text block", func(s *appSink) {
			s.Emit(ctx, turnevent.TextBlock{Text: "hi", Phase: turnevent.PhaseIntermediate})
		}, "typing"},
		{"thinking delta", func(s *appSink) { s.Emit(ctx, turnevent.ThinkingDelta{Delta: "hm"}) }, "thinking"},
		{"thinking block", func(s *appSink) { s.Emit(ctx, turnevent.ThinkingBlock{Text: "hm"}) }, "thinking"},
		{"tool call", func(s *appSink) {
			s.Emit(ctx, turnevent.ToolCall{Name: "Read", ID: "t2", Args: []byte(`{"file_path":"/x"}`)})
		}, "tool"},
		{"tool result", func(s *appSink) { s.Emit(ctx, turnevent.ToolResult{Name: "Read", ID: "t1"}) }, "warming"},
		{"turn complete", func(s *appSink) { s.Emit(ctx, turnevent.TurnComplete{FinalText: "done"}) }, "idle"},
		{"cleanup of an abandoned turn", func(s *appSink) { s.cleanup() }, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClient()
			b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
			s := newAppSink(b)
			s.Emit(ctx, turnevent.TurnStart{})
			s.Emit(ctx, turnevent.ToolCall{Name: "Read", ID: "t1", Args: []byte(`{"file_path":"/x"}`)})
			drain(t, c)

			s.Emit(ctx, testRetry)
			frames := activityFrames(drain(t, c))
			if len(frames) != 1 || frames[0]["kind"] != "retrying" || frames[0]["detail"] != testRetryText ||
				frames[0]["agentKind"] != "retrying" || frames[0]["agentDetail"] != testRetryText {
				t.Fatalf("frames after the retry = %v, want one retrying frame", frames)
			}
			if info := b.info(); info.Activity != "retrying" || info.ActivityDetail != testRetryText {
				t.Fatalf("roster = %q/%q, want retrying", info.Activity, info.ActivityDetail)
			}

			// Not a sign the main call went through: no frame, still retrying.
			s.Emit(ctx, turnevent.Activity{})
			s.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_a", Text: "sub", RunIndex: 1})
			if frames := activityFrames(drain(t, c)); len(frames) != 0 {
				t.Fatalf("heartbeat/subagent sent %v, want nothing", frames)
			}
			if b.info().Activity != "retrying" {
				t.Fatalf("heartbeat cleared the retry: %q", b.info().Activity)
			}

			tc.clear(s)
			frames = activityFrames(drain(t, c))
			if len(frames) != 1 || frames[0]["kind"] != tc.want {
				t.Fatalf("frames after clearing = %v, want exactly one %q frame", frames, tc.want)
			}
			if b.info().Activity != tc.want {
				t.Fatalf("roster after clearing = %q, want %q", b.info().Activity, tc.want)
			}
		})
	}
}

// TestConvBinding_RetryingPrecedence: subagents > waiting > retrying > the
// turn-scoped kind, and the kind under a retry is kept for when it clears.
func TestConvBinding_RetryingPrecedence(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	b.setTurnActivity(fap.ActivityKindThinking)
	b.setRetrying(testRetryText)
	if info := b.info(); info.Activity != "retrying" || info.ActivityDetail != testRetryText {
		t.Fatalf("retrying must outrank thinking: %+v", info)
	}
	b.setWaitingDetail("scout")
	if info := b.info(); info.Activity != "waiting" || info.ActivityAgentKind != "waiting" {
		t.Fatalf("waiting must outrank retrying: %+v", info)
	}
	b.setWaitingDetail("")
	b.setSubagentDetail("researcher")
	if info := b.info(); info.Activity != "subagents" || info.ActivityAgentKind != "retrying" || info.ActivityAgentDetail != testRetryText {
		t.Fatalf("with subagents running, want kind subagents and agentKind retrying: %+v", info)
	}
	b.setSubagentDetail("")
	if b.info().Activity != "retrying" {
		t.Fatalf("after subagents end: %q, want retrying", b.info().Activity)
	}
	got := activityFrames(drain(t, c))
	want := []string{"thinking", "retrying", "waiting", "retrying", "subagents", "retrying"}
	if len(got) != len(want) {
		t.Fatalf("frames = %v, want kinds %v", got, want)
	}
	for i := range want {
		if got[i]["kind"] != want[i] {
			t.Fatalf("frames = %v, want kinds %v", got, want)
		}
	}
}
