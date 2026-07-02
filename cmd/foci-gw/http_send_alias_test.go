package main

import (
	"testing"

	"foci/internal/session"
)

// Proves resolveSendSession's precedence: an existing named session wins; else a
// chat alias; else a new named session; else an error.
func TestResolveSendSession(t *testing.T) {
	idx, err := session.NewSessionIndex(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	// A chat aliased "holiday" → its session key.
	if err := idx.SetChatAliasUnique("clutch", "app", 7, "holiday"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatMetadata("clutch", "app", 7, "session_key", "clutch/c7/1000"); err != nil {
		t.Fatal(err)
	}

	// Alias wins when no named session of that name exists.
	if got, err := resolveSendSession(idx, "clutch", "holiday"); err != nil || got != "clutch/c7/1000" {
		t.Fatalf("alias: got %q, %v; want clutch/c7/1000", got, err)
	}

	// An existing named session of the same name wins over the alias.
	named, err := session.NamedIndependentSessionKey("clutch", "holiday")
	if err != nil {
		t.Fatal(err)
	}
	idx.Upsert(session.SessionIndexEntry{SessionKey: named, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	if got, err := resolveSendSession(idx, "clutch", "holiday"); err != nil || got != named {
		t.Fatalf("named-wins: got %q, %v; want %q", got, err, named)
	}

	// A fresh valid name with no alias and no existing session → the named key.
	fresh, _ := session.NamedIndependentSessionKey("clutch", "brandnew")
	if got, err := resolveSendSession(idx, "clutch", "brandnew"); err != nil || got != fresh {
		t.Fatalf("fresh-named: got %q, %v; want %q", got, err, fresh)
	}

	// An invalid session name with no matching alias → error.
	if _, err := resolveSendSession(idx, "clutch", "bad name!/x"); err == nil {
		t.Fatal("expected error for invalid name with no alias")
	}
}
