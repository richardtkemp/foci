package session

import (
	"reflect"
	"testing"
)

// TestOpenChatSessionKeys resolves the persisted open set the way the hub's
// ensureBinding does (#3134): a plain chat gets its derived chat key, a chat
// with a session_key row (an adopted session, a depth leaf) gets that key, a
// conversation that is not open is left out, and an open id with no conv_id
// row (no binding) is skipped.
func TestOpenChatSessionKeys(t *testing.T) {
	idx := tempIndex(t)
	set := func(agent string, chat int64, key, val string) {
		t.Helper()
		if err := idx.SetChatMetadata(agent, "app", chat, key, val); err != nil {
			t.Fatal(err)
		}
	}
	set("alpha", 1, "conv_id", "conv-plain")
	set("alpha", 2, "conv_id", "conv-leaf")
	set("alpha", 2, "session_key", "alpha/c1/b9")
	set("beta", 3, "conv_id", "conv-closed")
	// A telegram row with the same key name is not an app conversation.
	if err := idx.SetChatMetadata("beta", "telegram", 4, "conv_id", "conv-tg"); err != nil {
		t.Fatal(err)
	}

	got, err := idx.OpenChatSessionKeys()
	if err != nil || len(got) != 0 {
		t.Fatalf("unset open set = %v, %v; want empty, nil", got, err)
	}

	if err := idx.SetSystemState(SystemStateOpenChats, `["conv-plain","conv-leaf","conv-unbound","conv-tg"]`); err != nil {
		t.Fatal(err)
	}
	got, err = idx.OpenChatSessionKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{NewChatSessionKey("alpha", 1): true, "alpha/c1/b9": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("open keys = %v, want %v", got, want)
	}

	if err := idx.SetSystemState(SystemStateOpenChats, `{"not":"an array"}`); err != nil {
		t.Fatal(err)
	}
	if got, err := idx.OpenChatSessionKeys(); err == nil {
		t.Errorf("malformed open set = %v, nil; want an error", got)
	}
}
