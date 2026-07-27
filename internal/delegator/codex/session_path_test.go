package codex

import (
	"encoding/json"
	"testing"
)

func TestThreadStartedCapturesSessionFilePath(t *testing.T) {
	b := newTestBackend(t)
	b.dispatch([]byte(`{"method":"thread/started","params":{"thread":{"id":"thread-1","path":"/tmp/rollout.jsonl"}}}`))

	if got := b.SessionID(); got != "thread-1" {
		t.Fatalf("SessionID() = %q, want thread-1", got)
	}
	if got := b.SessionFilePath(); got != "/tmp/rollout.jsonl" {
		t.Fatalf("SessionFilePath() = %q, want /tmp/rollout.jsonl", got)
	}
}

func TestStartThreadCapturesSessionFilePath(t *testing.T) {
	b := newStartableBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "thread/start" {
			t.Fatalf("method = %q, want thread/start", method)
		}
		return json.RawMessage(`{"thread":{"id":"thread-1","path":"/tmp/rollout.jsonl"}}`), nil
	})

	if _, err := b.startThread(); err != nil {
		t.Fatalf("startThread: %v", err)
	}
	if got := b.SessionFilePath(); got != "/tmp/rollout.jsonl" {
		t.Fatalf("SessionFilePath() = %q, want /tmp/rollout.jsonl", got)
	}
}

func TestSessionFilePathEmptyUntilCodexReportsPath(t *testing.T) {
	b := newTestBackend(t)
	setTestThread(b, "thread-1")

	if got := b.SessionFilePath(); got != "" {
		t.Fatalf("SessionFilePath() = %q, want empty", got)
	}
}

func TestThreadStatusChangedUpdatesStatusDetail(t *testing.T) {
	b := newTestBackend(t)
	b.dispatch([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread-1","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}`))

	if got := b.StatusDetail(); got != "sandbox=workspace-write | codex=active (waitingOnApproval)" {
		t.Fatalf("StatusDetail() = %q, want Codex status", got)
	}
}

func TestThreadStartedCapturesStatus(t *testing.T) {
	b := newTestBackend(t)
	b.dispatch([]byte(`{"method":"thread/started","params":{"thread":{"id":"thread-1","status":{"type":"idle"}}}}`))

	if got := b.StatusDetail(); got != "sandbox=workspace-write | codex=idle" {
		t.Fatalf("StatusDetail() = %q, want Codex status", got)
	}
}

func TestThreadLogPrefixIncludesThreadID(t *testing.T) {
	b := newTestBackend(t)
	if got := b.threadLogPrefix(); got != "thread=<unknown>" {
		t.Fatalf("threadLogPrefix() = %q, want unknown marker", got)
	}
	setTestThread(b, "thread-123")
	if got := b.threadLogPrefix(); got != "thread=thread-123" {
		t.Fatalf("threadLogPrefix() = %q, want thread ID", got)
	}
}

func TestSessionIDDoesNotUseBackendGlobalThread(t *testing.T) {
	b := &Backend{threadID: "legacy-thread"}
	if got := b.SessionID(); got != "" {
		t.Fatalf("SessionID() = %q, want empty without explicit session mapping", got)
	}
}
