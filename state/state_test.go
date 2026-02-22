package state

import (
	"path/filepath"
	"testing"
)

func TestStoreSetGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Set a string value
	if err := s.Set("key1", "hello"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var val string
	if !s.Get("key1", &val) {
		t.Fatal("Get returned false for existing key")
	}
	if val != "hello" {
		t.Errorf("Get = %q, want %q", val, "hello")
	}
}

func TestStoreGetMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	var val string
	if s.Get("nonexistent", &val) {
		t.Error("Get returned true for missing key")
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	// Write
	s1 := New(path)
	if err := s1.Set("chatid", int64(12345)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s1.Set("voice", true); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read from a fresh store
	s2 := New(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var chatID int64
	if !s2.Get("chatid", &chatID) {
		t.Fatal("chatid not found after reload")
	}
	if chatID != 12345 {
		t.Errorf("chatid = %d, want 12345", chatID)
	}

	var voice bool
	if !s2.Get("voice", &voice) {
		t.Fatal("voice not found after reload")
	}
	if !voice {
		t.Error("voice = false, want true")
	}
}

func TestStoreLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	s := New(path)

	// Loading a missing file should not error
	if err := s.Load(); err != nil {
		t.Fatalf("Load of missing file: %v", err)
	}
}

func TestStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	s.Set("key1", "value1")
	s.Set("key2", "value2")

	if err := s.Delete("key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var val string
	if s.Get("key1", &val) {
		t.Error("key1 should be deleted")
	}
	if !s.Get("key2", &val) {
		t.Error("key2 should still exist")
	}
}

func TestStoreStructValue(t *testing.T) {
	type WatchConfig struct {
		Session   string `json:"session"`
		Window    int    `json:"window"`
		Threshold int    `json:"threshold"`
	}

	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	watches := []WatchConfig{
		{Session: "dev", Window: 0, Threshold: 30},
		{Session: "build", Window: 1, Threshold: 60},
	}
	if err := s.Set("tmux_watches", watches); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Reload
	s2 := New(path)
	s2.Load()

	var loaded []WatchConfig
	if !s2.Get("tmux_watches", &loaded) {
		t.Fatal("tmux_watches not found")
	}
	if len(loaded) != 2 {
		t.Fatalf("len = %d, want 2", len(loaded))
	}
	if loaded[0].Session != "dev" || loaded[1].Threshold != 60 {
		t.Errorf("loaded = %+v", loaded)
	}
}
