package telegram

import (
	"errors"
	"testing"
	"time"

	"foci/internal/log"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// flood429 builds a Telegram flood-control error carrying the given Retry-After.
func flood429(retryAfter int64) error {
	return &gotgbot.TelegramError{
		Method:         "sendMessage",
		Code:           429,
		Description:    "Too Many Requests: flood control exceeded",
		ResponseParams: &gotgbot.ResponseParameters{RetryAfter: retryAfter},
	}
}

// withStubbedSleep replaces floodSleep for the duration of a test, recording the
// waits requested so the retry timing can be asserted without real blocking.
func withStubbedSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	orig := floodSleep
	var waits []time.Duration
	floodSleep = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { floodSleep = orig })
	return &waits
}

func retryTestBot() *Bot {
	return &Bot{log: log.NewComponentLogger("telegram:test")}
}

func TestRetryOn429_SucceedsAfterRetries(t *testing.T) {
	waits := withStubbedSleep(t)
	b := retryTestBot()

	calls := 0
	err := b.retryOn429("send", func() error {
		calls++
		if calls < 3 {
			return flood429(2) // 429 on first two attempts, success on third
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 send attempts, got %d", calls)
	}
	// Two retries → two waits, each Retry-After (2s) + 1s pad = 3s.
	if len(*waits) != 2 {
		t.Fatalf("expected 2 waits, got %d (%v)", len(*waits), *waits)
	}
	for i, w := range *waits {
		if w != 3*time.Second {
			t.Errorf("wait[%d] = %v, want 3s", i, w)
		}
	}
}

func TestRetryOn429_GivesUpAfterMax(t *testing.T) {
	waits := withStubbedSleep(t)
	b := retryTestBot()

	calls := 0
	err := b.retryOn429("send", func() error {
		calls++
		return flood429(1) // always flood-limited
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// Initial attempt + maxFloodRetries retries.
	if calls != maxFloodRetries+1 {
		t.Fatalf("expected %d attempts, got %d", maxFloodRetries+1, calls)
	}
	if len(*waits) != maxFloodRetries {
		t.Fatalf("expected %d waits, got %d", maxFloodRetries, len(*waits))
	}
}

func TestRetryOn429_NonFloodErrorReturnsImmediately(t *testing.T) {
	waits := withStubbedSleep(t)
	b := retryTestBot()

	sentinel := errors.New("bad request: malformed entities")
	calls := 0
	err := b.retryOn429("send", func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error returned verbatim, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("non-429 must not retry: got %d attempts", calls)
	}
	if len(*waits) != 0 {
		t.Fatalf("non-429 must not sleep: got %v", *waits)
	}
}

func TestRetryOn429_429WithoutRetryAfterDoesNotRetry(t *testing.T) {
	withStubbedSleep(t)
	b := retryTestBot()

	// A 429 with no ResponseParams / zero Retry-After is not actionable — we
	// have no instructed wait, so return immediately rather than busy-loop.
	calls := 0
	err := b.retryOn429("send", func() error {
		calls++
		return &gotgbot.TelegramError{Code: 429, Description: "Too Many Requests"}
	})
	if err == nil {
		t.Fatal("expected the 429 error to be returned")
	}
	if calls != 1 {
		t.Fatalf("429 without Retry-After must not retry: got %d attempts", calls)
	}
}

func TestRetryOn429_WaitCappedAtMax(t *testing.T) {
	waits := withStubbedSleep(t)
	b := retryTestBot()

	calls := 0
	_ = b.retryOn429("send", func() error {
		calls++
		if calls == 1 {
			return flood429(int64(10 * time.Minute / time.Second)) // absurd Retry-After
		}
		return nil
	})
	if len(*waits) != 1 {
		t.Fatalf("expected 1 wait, got %d", len(*waits))
	}
	if (*waits)[0] != maxFloodWait {
		t.Errorf("wait = %v, want capped at %v", (*waits)[0], maxFloodWait)
	}
}
