package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// TestMaybeInjectCompactionResume covers the #845 post-compaction resume nudge:
// after a delegated compaction bounce, foci self-injects a "resume if you were
// mid-task, else no response" prompt so an interrupted flow continues — UNLESS
// the user already queued a follow-up (which will drive continuation itself) or
// async self-injection is unavailable.
func TestMaybeInjectCompactionResume(t *testing.T) {
	t.Parallel()
	const sk = "test/s"

	newNotifier := func(got *[]string, mu *sync.Mutex) *tools.AsyncNotifier {
		return tools.NewAsyncNotifier(func(_ string, msg, _, _ string, _ *tools.AgentOrigin) {
			mu.Lock()
			defer mu.Unlock()
			*got = append(*got, msg)
		})
	}

	t.Run("idle session injects resume nudge", func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		a := &Agent{AsyncNotifier: newNotifier(&got, &mu)}

		a.maybeInjectCompactionResume(context.Background(), sk)

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("expected 1 inject for idle session, got %d: %v", len(got), got)
		}
		if !strings.Contains(strings.ToLower(got[0]), "resume") {
			t.Errorf("inject missing resume instruction: %q", got[0])
		}
	})

	t.Run("queued user message suppresses nudge", func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		a := &Agent{AsyncNotifier: newNotifier(&got, &mu)}
		inb := a.getOrCreateInbox(sk)
		inb.ch <- Envelope{SessionKey: sk, Text: "do the next thing"}

		a.maybeInjectCompactionResume(context.Background(), sk)

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 0 {
			t.Fatalf("expected no inject when user input queued, got %v", got)
		}
	})

	t.Run("buffered steer suppresses nudge", func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		a := &Agent{AsyncNotifier: newNotifier(&got, &mu)}
		inb := a.getOrCreateInbox(sk)
		inb.appendSteer(SteerEntry{Text: "mid-flow paste", ReceivedAt: time.Now()})

		a.maybeInjectCompactionResume(context.Background(), sk)

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 0 {
			t.Fatalf("expected no inject when steer buffered, got %v", got)
		}
	})

	t.Run("nil notifier is a no-op", func(t *testing.T) {
		a := &Agent{}
		a.maybeInjectCompactionResume(context.Background(), sk) // must not panic
	})
}

// TestCompactionResumePromptToolFirst covers #3093: on a backend whose Spec sets
// PostCompactNeedsToolCall (Claude Code), the idle branch of the resume nudge
// makes one trivial tool call before the no-response sentinel, because a
// text-only first reply after a compaction leaves the cached tail unreadable to
// every later request (#3086). Other backends keep the text-only prompt.
func TestCompactionResumePromptToolFirst(t *testing.T) {
	t.Parallel()
	const sk = "test/s"
	inject := func(spec delegator.Spec, withManager bool) string {
		var mu sync.Mutex
		var got []string
		a := &Agent{AsyncNotifier: tools.NewAsyncNotifier(func(_ string, msg, _, _ string, _ *tools.AgentOrigin) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, msg)
		})}
		if withManager {
			a.DelegatedManager = &DelegatedManager{Spec: spec}
		}
		a.maybeInjectCompactionResume(context.Background(), sk)
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("expected 1 inject, got %d: %v", len(got), got)
		}
		return got[0]
	}

	t.Run("tool-call backend asks for a tool call before the sentinel", func(t *testing.T) {
		got := inject(delegator.Spec{PostCompactNeedsToolCall: true}, true)
		tool := strings.Index(got, "foci_whoami")
		if tool < 0 {
			t.Fatalf("prompt names no trivial tool call: %q", got)
		}
		if sent := strings.LastIndex(got, NoResponseSentinel); sent < tool {
			t.Errorf("sentinel must come after the tool call: %q", got)
		}
		if !strings.Contains(strings.ToLower(got), "resume") {
			t.Errorf("prompt lost the resume instruction: %q", got)
		}
	})

	t.Run("other backends keep the text-only prompt", func(t *testing.T) {
		for name, tc := range map[string]struct {
			spec delegator.Spec
			mgr  bool
		}{"zero spec": {delegator.Spec{}, true}, "no manager": {delegator.Spec{}, false}} {
			if got := inject(tc.spec, tc.mgr); got != compactionResumePrompt {
				t.Errorf("%s: got %q, want the unchanged prompt", name, got)
			}
			if got := inject(tc.spec, tc.mgr); strings.Contains(got, "foci_whoami") {
				t.Errorf("%s: text-only prompt asks for a tool call: %q", name, got)
			}
		}
	})
}
