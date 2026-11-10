package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCompactSession_RequestsForCallingSession (#3040): the tool hands the
// requester the calling session's key and says the compaction runs after the
// turn.
func TestCompactSession_RequestsForCallingSession(t *testing.T) {
	t.Parallel()
	var got []string
	tool := NewCompactSessionTool(func(sk, _ string) error {
		got = append(got, sk)
		return nil
	})
	res, err := tool.Execute(WithSessionKey(context.Background(), "clutch/app/c1"), []byte(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 || got[0] != "clutch/app/c1" {
		t.Fatalf("requester got %q, want the calling session", got)
	}
	if !strings.Contains(res.Text, "when this turn ends") {
		t.Errorf("result %q does not say the compaction runs after the turn", res.Text)
	}
}

// TestCompactSession_Refusals: no session key, a character spawn (its calls
// run on the parent's context) and a requester error are all errors, and
// only the last reaches the requester.
func TestCompactSession_Refusals(t *testing.T) {
	t.Parallel()
	calls := 0
	reqErr := errors.New("compaction is not configured for this agent")
	tool := NewCompactSessionTool(func(string, string) error {
		calls++
		return reqErr
	})

	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Error("no session key: want an error")
	}
	spawnCtx := WithSpawnCharacter(WithSessionKey(context.Background(), "clutch/app/c1"))
	if _, err := tool.Execute(spawnCtx, nil); err == nil || !strings.Contains(err.Error(), "character spawn") {
		t.Errorf("character spawn: err = %v, want a refusal naming it", err)
	}
	if calls != 0 {
		t.Fatalf("requester called %d times by refused calls", calls)
	}
	_, err := tool.Execute(WithSessionKey(context.Background(), "clutch/app/c1"), nil)
	if !errors.Is(err, reqErr) {
		t.Errorf("err = %v, want the requester's error wrapped", err)
	}
}

// TestCompactSession_WakeMessage (#3041): wake_message reaches the requester
// trimmed, and the result says the wake follows; without it the result says
// nothing wakes the agent.
func TestCompactSession_WakeMessage(t *testing.T) {
	t.Parallel()
	var gotWake string
	tool := NewCompactSessionTool(func(_, wake string) error {
		gotWake = wake
		return nil
	})
	ctx := WithSessionKey(context.Background(), "clutch/app/c1")
	res, err := tool.Execute(ctx, []byte(`{"wake_message":"  continue step 2  "}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotWake != "continue step 2" {
		t.Errorf("requester got wake %q, want it trimmed", gotWake)
	}
	if !strings.Contains(res.Text, "wake message is sent") {
		t.Errorf("result %q does not say the wake follows", res.Text)
	}
	res, err = tool.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotWake != "" || !strings.Contains(res.Text, "Nothing wakes you") {
		t.Errorf("no wake_message: requester got %q, result %q", gotWake, res.Text)
	}
}
