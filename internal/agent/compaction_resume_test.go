package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/tools"
)

// TestMaybeInjectCompactionResume covers the #845 post-compaction resume nudge:
// after a delegated compaction bounce, foci self-injects a "resume if you were
// mid-task, else no response" prompt so an interrupted flow continues — UNLESS
// the user already queued a follow-up (which will drive continuation itself) or
// async self-injection is unavailable.
func TestMaybeInjectCompactionResume(t *testing.T) {
	const sk = "test/s"

	newNotifier := func(got *[]string, mu *sync.Mutex) *tools.AsyncNotifier {
		return tools.NewAsyncNotifier(func(_ string, msg, _, _ string) {
			mu.Lock()
			defer mu.Unlock()
			*got = append(*got, msg)
		})
	}

	t.Run("idle session injects resume nudge", func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		a := &Agent{AsyncNotifier: newNotifier(&got, &mu)}

		a.maybeInjectCompactionResume(sk)

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

		a.maybeInjectCompactionResume(sk)

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
		inb.appendSteer("mid-flow paste", time.Now())

		a.maybeInjectCompactionResume(sk)

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 0 {
			t.Fatalf("expected no inject when steer buffered, got %v", got)
		}
	})

	t.Run("nil notifier is a no-op", func(t *testing.T) {
		a := &Agent{}
		a.maybeInjectCompactionResume(sk) // must not panic
	})
}
