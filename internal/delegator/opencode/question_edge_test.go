package opencode

import (
	"encoding/json"
	"sync"
	"testing"
)

// --- #159: 5 edge-case question tests ported from ccstream era ---
//
// The ccstream model had a single AskUserQuestion tool call containing
// multiple sequential questions. opencode's model is simpler: each
// question is an independent permission event. These tests adapt the
// edge cases to opencode's per-permission question model.

func TestRespondToQuestion_MultipleQuestionsAnsweredIndependently(t *testing.T) {
	// Port of TestRespondToQuestion_SequentialMultiQuestion.
	// opencode: two separate question permissions, each answered independently.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{
		Header: "Survey", Text: "Pick one",
		Options: []questionOption{{Label: "A"}, {Label: "B"}},
	})

	// Two independent questions arrive.
	b.onPermissionUpdated(Permission{
		ID: "perm-q1", Type: PermQuestion, Title: "Q1",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})
	b.onPermissionUpdated(Permission{
		ID: "perm-q2", Type: PermQuestion, Title: "Q2",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	// Both should be pending.
	if b.HasPendingQuestion() == "" {
		t.Fatal("expected at least one pending question")
	}

	// Answer Q1.
	if err := b.RespondToQuestion("perm-q1", "A"); err != nil {
		t.Fatalf("RespondToQuestion Q1: %v", err)
	}
	post1, _ := rec.lastPermPost()
	var body1 struct{ Response string `json:"response"` }
	json.Unmarshal(post1.Body, &body1)
	if body1.Response != "A" {
		t.Errorf("Q1 response = %q, want A", body1.Response)
	}

	// Q2 should still be pending after Q1 resolved.
	if id := b.HasPendingQuestion(); id == "" {
		t.Error("Q2 should still be pending after Q1 answered")
	}

	// Answer Q2.
	if err := b.RespondToQuestion("perm-q2", "B"); err != nil {
		t.Fatalf("RespondToQuestion Q2: %v", err)
	}

	// No pending questions remain.
	if id := b.HasPendingQuestion(); id != "" {
		t.Errorf("HasPendingQuestion = %q, want empty after all answered", id)
	}
}

func TestCancelQuestion_OneOfMultipleRemains(t *testing.T) {
	// Port of TestCancelQuestion_MidSequence.
	// opencode: cancel one question, verify the other is still answerable.
	b, rec := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{Header: "H", Text: "?"})

	b.onPermissionUpdated(Permission{
		ID: "perm-keep", Type: PermQuestion, Title: "Keep me",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})
	b.onPermissionUpdated(Permission{
		ID: "perm-cancel", Type: PermQuestion, Title: "Cancel me",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	// Cancel one.
	if err := b.CancelQuestion("perm-cancel"); err != nil {
		t.Fatalf("CancelQuestion: %v", err)
	}
	post, _ := rec.lastPermPost()
	var body struct{ Response string `json:"response"` }
	json.Unmarshal(post.Body, &body)
	if body.Response != "deny" {
		t.Errorf("cancelled response = %q, want deny", body.Response)
	}

	// The other should still be pending and answerable.
	if id := b.HasPendingQuestion(); id != "perm-keep" {
		t.Errorf("HasPendingQuestion = %q, want perm-keep", id)
	}
	if err := b.RespondToQuestion("perm-keep", "yes"); err != nil {
		t.Errorf("RespondToQuestion perm-keep after cancel: %v", err)
	}
}

func TestRespondToQuestion_AlreadyAnsweredReturnsError(t *testing.T) {
	// Port of TestRespondToQuestion_AlreadyAnswered.
	// Double-response idempotency: second response should error.
	b, _ := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{Header: "H", Text: "?", Options: []questionOption{{Label: "X"}}})
	b.onPermissionUpdated(Permission{
		ID: "perm-dup", Type: PermQuestion, Title: "?",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	// First answer succeeds.
	if err := b.RespondToQuestion("perm-dup", "X"); err != nil {
		t.Fatalf("first RespondToQuestion: %v", err)
	}

	// Second should fail — permission was deleted from pendingPerms.
	if err := b.RespondToQuestion("perm-dup", "X"); err == nil {
		t.Error("expected error for already-answered question, got nil")
	}
}

func TestRespondToQuestion_OnNonQuestionPermissionReturnsError(t *testing.T) {
	// Port of TestRespondToQuestion_InvalidOptionIndex.
	// opencode has no index-based selection; the equivalent edge case
	// is calling RespondToQuestion on a non-question permission.
	b, _ := newPermTestBackend(t)
	b.onPermissionUpdated(Permission{
		ID: "perm-bash", Type: PermBash, Title: "run ls",
		SessionID: "sess-perm", MessageID: "m", Metadata: json.RawMessage(`{}`),
	})

	err := b.RespondToQuestion("perm-bash", "anything")
	if err == nil {
		t.Error("expected error calling RespondToQuestion on a non-question permission")
	}
}

func TestRespondToQuestion_ConcurrentAccessNoRace(t *testing.T) {
	// Port of TestRespondToQuestion_ConcurrentAccess.
	// Concurrent RespondToQuestion + HasPendingQuestion must not race
	// (verified by -race flag).
	b, _ := newPermTestBackend(t)
	meta, _ := json.Marshal(questionMetadata{Header: "H", Text: "?", Options: []questionOption{{Label: "A"}}})
	b.onPermissionUpdated(Permission{
		ID: "perm-conc", Type: PermQuestion, Title: "?",
		SessionID: "sess-perm", MessageID: "m", Metadata: meta,
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = b.HasPendingQuestion()
	}()
	go func() {
		defer wg.Done()
		_ = b.RespondToQuestion("perm-conc", "A")
	}()

	wg.Wait()
}
