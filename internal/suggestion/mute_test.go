package suggestion

import (
	"path/filepath"
	"sync"
	"testing"

	"foci/internal/session"
)

func tempMuteIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "test_index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// TestMuteStoresSortedDeduplicatedJSONArray proves the stored form: a JSON
// array, sorted and de-duplicated, so equal sets never differ on the wire.
func TestMuteStoresSortedDeduplicatedJSONArray(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)

	if err := m.mute("clutch", 42, "z.suggestion"); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if err := m.mute("clutch", 42, "a.suggestion"); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if err := m.mute("clutch", 42, "a.suggestion"); err != nil { // duplicate collapses
		t.Fatalf("mute: %v", err)
	}

	stored, err := idx.GetChatMetadata("clutch", "", 42, MuteKey)
	if err != nil {
		t.Fatalf("GetChatMetadata: %v", err)
	}
	if stored != `["a.suggestion","z.suggestion"]` {
		t.Errorf("stored = %s, want [\"a.suggestion\",\"z.suggestion\"]", stored)
	}
}

// TestMuteUnreadableStoredValueTreatedAsEmpty proves the recovery contract: a
// value that cannot be decoded reads as "nothing muted" (Send proceeds) and
// is overwritten by the next mute rather than wedging the chat.
func TestMuteUnreadableStoredValueTreatedAsEmpty(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)

	if err := idx.SetChatMetadata("clutch", "", 42, MuteKey, `{"not":"an array"}`); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	muted, err := m.muted("clutch", 42, "a.suggestion")
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("muted(garbage stored) = true, want false")
	}

	if err := m.mute("clutch", 42, "a.suggestion"); err != nil {
		t.Fatalf("mute over garbage: %v", err)
	}
	stored, err := idx.GetChatMetadata("clutch", "", 42, MuteKey)
	if err != nil {
		t.Fatalf("GetChatMetadata: %v", err)
	}
	if stored != `["a.suggestion"]` {
		t.Errorf("stored after overwrite = %s, want [\"a.suggestion\"]", stored)
	}
}

// TestMuteOtherSuggestionUnaffected proves mutes are per suggestion id: muting one id
// leaves another sendable for the same chat.
func TestMuteOtherSuggestionUnaffected(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)

	if err := m.mute("clutch", 42, "a.suggestion"); err != nil {
		t.Fatalf("mute: %v", err)
	}
	muted, err := m.muted("clutch", 42, "b.suggestion")
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("muting a.suggestion muted b.suggestion too")
	}
}

// TestMuteConcurrentPressesBothPersist proves the read-modify-write is
// serialised: two goroutines muting different ids at the same time both land
// in the stored set (the same guarantee Hub.pinsMu gives pins).
func TestMuteConcurrentPressesBothPersist(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		id := string(rune('a'+i)) + ".suggestion"
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := m.mute("clutch", 42, id); err != nil {
				t.Errorf("mute %s: %v", id, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	stored, err := idx.GetChatMetadata("clutch", "", 42, MuteKey)
	if err != nil {
		t.Fatalf("GetChatMetadata: %v", err)
	}
	want := `["a.suggestion","b.suggestion","c.suggestion","d.suggestion","e.suggestion","f.suggestion","g.suggestion","h.suggestion"]`
	if stored != want {
		t.Errorf("stored = %s, want %s", stored, want)
	}
}

// TestMuteSurvivesPlatformRowAppearingLater is the F1 regression: a mute is
// keyed by (agent, chatID) alone — the index's platform-agnostic bucket — so
// a mute stored BEFORE the chat has a registered platform row is still the
// one a later read finds AFTER that row exists. Keying by the resolved
// platform (or falling back to platform="") would move the mute into a bucket
// nothing reads once the platform row appears.
func TestMuteSurvivesPlatformRowAppearingLater(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)

	// No platform row exists yet.
	if err := m.mute("clutch", 42, "a.suggestion"); err != nil {
		t.Fatalf("mute before platform row: %v", err)
	}

	// The chat gets its registered platform row (the chatmeta.Resolver
	// writes one on first contact, backing PlatformForChat).
	if err := idx.SetChatMetadata("clutch", "telegram", 42, "registered", "clutch"); err != nil {
		t.Fatalf("seed platform row: %v", err)
	}

	muted, err := m.muted("clutch", 42, "a.suggestion")
	if err != nil {
		t.Fatalf("muted after platform row: %v", err)
	}
	if !muted {
		t.Error("mute was lost when the platform row appeared")
	}
}

// TestMuteWriteFailurePropagates proves a mute that could not be stored is
// reported (and so can be surfaced honestly instead of silently dropped).
func TestMuteWriteFailurePropagates(t *testing.T) {
	idx := tempMuteIndex(t)
	m := newMuteStore(idx)
	if err := idx.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
	if err := m.mute("clutch", 42, "a.suggestion"); err == nil {
		t.Error("mute on a closed index = nil, want error")
	}
}
