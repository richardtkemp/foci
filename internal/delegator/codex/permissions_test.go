package codex

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

func newPermTestBackend(buf *bytes.Buffer) *Backend {
	b := newTestBackend(&testing.T{})
	b.writer = NewWriter(nopWriteCloser{buf})
	return b
}

func parseApprovalResponse(t *testing.T, raw string) (id int64, decision string) {
	t.Helper()
	var env struct {
		ID     int64 `json:"id"`
		Result struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &env); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, raw)
	}
	return env.ID, env.Result.Decision
}

func TestOnCommandApproval_FiresPermPromptFn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	var (
		gotReqID, gotSummary string
		gotChoices           []delegator.PromptChoice
	)
	b.permPromptFn = func(itemID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		gotReqID, gotSummary, gotChoices = itemID, summary, choices
	}

	b.onCommandApproval([]byte(`{"itemId":"item_cmd","threadId":"th_1","turnId":"tn_1","reason":"dangerous","command":"rm -rf /","cwd":"/tmp"}`), 42)

	if gotReqID != "item_cmd" {
		t.Errorf("itemID = %q, want item_cmd", gotReqID)
	}
	if gotSummary != "dangerous" {
		t.Errorf("summary = %q, want dangerous", gotSummary)
	}
	if len(gotChoices) != 2 || gotChoices[0].Data != "allow" || gotChoices[1].Data != "deny" {
		t.Errorf("choices = %+v, want allow/deny", gotChoices)
	}
	b.permMu.Lock()
	p := b.pendingPerms[42]
	b.permMu.Unlock()
	if p == nil || p.command != "rm -rf /" {
		t.Errorf("pending approval not tracked correctly: %+v", p)
	}
	if buf.Len() != 0 {
		t.Errorf("no wire output expected while prompting, got %q", buf.String())
	}
}

func TestOnCommandApproval_DefaultSummaryWithoutReason(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	var gotSummary string
	b.permPromptFn = func(itemID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		gotSummary = summary
	}

	b.onCommandApproval([]byte(`{"itemId":"item_cmd2","threadId":"th","turnId":"tn","command":"echo hi"}`), 7)

	if gotSummary != "Run: echo hi" {
		t.Errorf("summary = %q, want %q", gotSummary, "Run: echo hi")
	}
}

func TestOnCommandApproval_AutoDenyWhenNoPermPromptFn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)

	b.onCommandApproval([]byte(`{"itemId":"item_auto","threadId":"th","turnId":"tn","command":"rm -rf /"}`), 99)

	id, decision := parseApprovalResponse(t, buf.String())
	if id != 99 || decision != "decline" {
		t.Errorf("response = %d/%q, want 99/deny", id, decision)
	}
}

func TestOnCommandApproval_MalformedDrops(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	b.permPromptFn = func(string, string, string, string, []delegator.PromptChoice) {
		t.Error("permPromptFn should not be called for malformed input")
	}

	b.onCommandApproval([]byte(`not json`), 5)

	if buf.Len() != 0 {
		t.Errorf("expected no wire output, got %q", buf.String())
	}
}

func TestOnFileChangeApproval_FiresPermPromptFn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	var gotReqID, gotSummary string
	b.permPromptFn = func(itemID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		gotReqID, gotSummary = itemID, summary
	}

	b.onFileChangeApproval([]byte(`{"itemId":"item_fc","threadId":"th_1","turnId":"tn_1","reason":"writing to config"}`), 13)

	if gotReqID != "item_fc" {
		t.Errorf("itemID = %q, want item_fc", gotReqID)
	}
	if gotSummary != "writing to config" {
		t.Errorf("summary = %q, want 'writing to config'", gotSummary)
	}
}

func TestOnFileChangeApproval_AutoDenyWhenNoPermPromptFn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)

	b.onFileChangeApproval([]byte(`{"itemId":"item_fc_auto","threadId":"th","turnId":"tn"}`), 77)

	id, decision := parseApprovalResponse(t, buf.String())
	if id != 77 || decision != "decline" {
		t.Errorf("response = %d/%q, want 77/deny", id, decision)
	}
}

func TestOnPermissionApproval_AlwaysDenies(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)

	b.onPermissionApproval([]byte(`{}`), 321)

	id, decision := parseApprovalResponse(t, buf.String())
	if id != 321 || decision != "decline" {
		t.Errorf("response = %d/%q, want 321/deny", id, decision)
	}
}

func TestRespondApproval_SendsResponseAndCleansUp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	b.permMu.Lock()
	b.pendingPerms[100] = &pendingApproval{rpcID: 100, itemID: "item_a", command: "ls"}
	b.permMu.Unlock()

	b.respondApproval(100, "accept")

	id, decision := parseApprovalResponse(t, buf.String())
	if id != 100 || decision != "accept" {
		t.Errorf("response = %d/%q, want 100/allow", id, decision)
	}
	b.permMu.Lock()
	_, stillThere := b.pendingPerms[100]
	b.permMu.Unlock()
	if stillThere {
		t.Error("pending entry should be removed")
	}
}

func TestRespondApproval_FiresOnPromptsCleared(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	cleared := 0
	b.onPromptsCleared = func() { cleared++ }
	b.permMu.Lock()
	b.pendingPerms[1] = &pendingApproval{rpcID: 1, itemID: "a"}
	b.pendingPerms[2] = &pendingApproval{rpcID: 2, itemID: "b"}
	b.permMu.Unlock()

	b.respondApproval(1, "decline")
	if cleared != 0 {
		t.Errorf("cleared = %d after first resolve, want 0", cleared)
	}

	b.respondApproval(2, "decline")
	if cleared != 1 {
		t.Errorf("cleared = %d after last resolve, want 1", cleared)
	}
}

func TestRespondToPermission_ResolvesByItemID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	b.permMu.Lock()
	b.pendingPerms[200] = &pendingApproval{rpcID: 200, itemID: "item_x", command: "git push"}
	b.permMu.Unlock()

	if err := b.RespondToPermission("item_x", false, "User denied permission"); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	id, decision := parseApprovalResponse(t, buf.String())
	if id != 200 || decision != "decline" {
		t.Errorf("response = %d/%q, want 200/deny", id, decision)
	}
}

func TestRespondToPermission_UnknownItemID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)

	err := b.RespondToPermission("ghost", false, "")
	if err == nil || !strings.Contains(err.Error(), "no pending approval") {
		t.Fatalf("expected no-pending error, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no wire output, got %q", buf.String())
	}
}
