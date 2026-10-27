package codex

import (
	"encoding/json"
	"testing"
)

// TestItemStarted_FileChangeKindIsAnObject: codex app-server (verified against
// the 0.164.0 generated schema, FileUpdateChange.kind → PatchChangeKind) sends
// each change's kind as an object {"type":"add"|"delete"|"update",
// "move_path":...}, not a string. Decoding it as a string made every real
// fileChange item fail to unmarshal, so onItemStarted dropped it and the
// approval prompt never knew which paths it was writing.
func TestItemStarted_FileChangeKindIsAnObject(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.itemCache = make(map[string]itemEnvelope)
	raw := json.RawMessage(`{"type":"fileChange","id":"item_fc","status":"inProgress","changes":[
		{"path":"/w/new.go","kind":{"type":"add"},"diff":"+x"},
		{"path":"/w/old.go","kind":{"type":"update","move_path":"/w/moved.go"},"diff":"-x"},
		{"path":"/w/gone.go","kind":{"type":"delete"},"diff":""}]}`)
	b.onItemStarted(&itemStartedParams{Item: raw})

	b.itemMu.Lock()
	got := b.itemCache["item_fc"].Changes
	b.itemMu.Unlock()
	if len(got) != 3 {
		t.Fatalf("cached changes = %+v, want 3 (item dropped as malformed?)", got)
	}
	if got[0].Kind.Type != "add" || got[1].Kind.Type != "update" || got[2].Kind.Type != "delete" {
		t.Errorf("kinds = %q %q %q, want add update delete", got[0].Kind.Type, got[1].Kind.Type, got[2].Kind.Type)
	}
	if got[1].Kind.MovePath != "/w/moved.go" {
		t.Errorf("move_path = %q, want /w/moved.go", got[1].Kind.MovePath)
	}
}
