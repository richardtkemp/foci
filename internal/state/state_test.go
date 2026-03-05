package state

import (
	"os"
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

func TestAllKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	s.Set("alpha", "a")
	s.Set("beta", "b")
	s.Set("gamma", "c")

	keys := s.AllKeys()
	if len(keys) != 3 {
		t.Fatalf("AllKeys = %d keys, want 3", len(keys))
	}

	// Check that all expected keys are present
	keyMap := make(map[string]bool)
	for _, k := range keys {
		keyMap[k] = true
	}
	if !keyMap["alpha"] || !keyMap["beta"] || !keyMap["gamma"] {
		t.Errorf("AllKeys missing expected keys: %v", keys)
	}
}

func TestAllKeys_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	keys := s.AllKeys()
	if len(keys) != 0 {
		t.Fatalf("AllKeys on empty store = %d, want 0", len(keys))
	}
}

func TestLoadCorruptedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Write corrupted JSON
	if err := os.WriteFile(path, []byte("{invalid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := New(path)
	err := s.Load()
	if err == nil {
		t.Fatal("Load should error on corrupted JSON")
	}
	if err.Error() != "parse state file: invalid character 'i' looking for beginning of object key string" {
		// Just check that it's a parse error
		if !contains(err.Error(), "parse") {
			t.Errorf("Load error = %v, want parse error", err)
		}
	}
}

func TestGetInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Manually insert invalid JSON into the data map
	s.data["bad"] = []byte("not valid json for the type")

	var val int
	result := s.Get("bad", &val)
	if result {
		t.Error("Get should return false when value is invalid JSON")
	}
}

func TestMultipleTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	s.Set("str", "hello")
	s.Set("num", 42)
	s.Set("bool", true)
	s.Set("arr", []int{1, 2, 3})

	var str string
	var num int
	var b bool
	var arr []int

	if !s.Get("str", &str) || str != "hello" {
		t.Error("string value incorrect")
	}
	if !s.Get("num", &num) || num != 42 {
		t.Error("number value incorrect")
	}
	if !s.Get("bool", &b) || !b {
		t.Error("bool value incorrect")
	}
	if !s.Get("arr", &arr) || len(arr) != 3 {
		t.Error("array value incorrect")
	}
}

func TestSetPersistenceMultiple(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Set multiple values
	s.Set("key1", 100)
	s.Set("key2", "text")
	s.Set("key3", true)

	// Create new store and reload
	s2 := New(path)
	s2.Load()

	var v1 int
	var v2 string
	var v3 bool

	if !s2.Get("key1", &v1) || v1 != 100 {
		t.Error("key1 not persisted correctly")
	}
	if !s2.Get("key2", &v2) || v2 != "text" {
		t.Error("key2 not persisted correctly")
	}
	if !s2.Get("key3", &v3) || !v3 {
		t.Error("key3 not persisted correctly")
	}
}

func TestDeleteNonexistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Deleting non-existent key should not error
	if err := s.Delete("nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestSetUnmarshalError tests Set when the value can't be marshaled
func TestSetUnmarshalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Try to marshal a channel (which can't be marshaled to JSON)
	ch := make(chan int)
	err := s.Set("channel", ch)
	if err == nil {
		t.Error("Set with unmarshalable value should return error")
	}
}

// TestLoadWriteError tests Set when file write fails
func TestLoadWriteError(t *testing.T) {
	// Use a read-only directory to trigger write error
	path := filepath.Join(t.TempDir(), "readonly", "state.json")
	os.MkdirAll(filepath.Dir(path), 0555) // read-only

	s := New(path)
	err := s.Set("key", "value")

	// Should get a write error
	if err == nil {
		t.Error("Set to read-only directory should return error")
	}

	// Clean up
	os.Chmod(filepath.Dir(path), 0755)
}

// TestGetWithUnmarshalError tests Get when stored data is corrupted
func TestGetWithUnmarshalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Manually insert corrupted JSON
	s.mu.Lock()
	s.data["corrupted"] = []byte("not valid json for int")
	s.mu.Unlock()

	var val int
	result := s.Get("corrupted", &val)

	// Should return false and log warning
	if result {
		t.Error("Get with corrupted data should return false")
	}
}

// TestLoadIntoExistingData tests Load overwrites existing data
func TestLoadIntoExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	// First write
	s1 := New(path)
	s1.Set("key1", "value1")

	// Load with new store - should have only what was written
	s2 := New(path)
	s2.Load()

	var val string
	if !s2.Get("key1", &val) {
		t.Error("should load previously saved value")
	}
	if val != "value1" {
		t.Errorf("value = %q, want %q", val, "value1")
	}
}

// TestDeleteThenGet tests that deleted keys are gone
func TestDeleteThenGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	// Set and delete
	s.Set("key", "value")
	s.Delete("key")

	// Reload and check it's gone
	s2 := New(path)
	s2.Load()

	var val string
	if s2.Get("key", &val) {
		t.Error("deleted key should not be found after reload")
	}
}

// TestSetMultipleUpdatesSameKey tests overwriting a key
func TestSetMultipleUpdatesSameKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(path)

	s.Set("key", "value1")
	s.Set("key", "value2")
	s.Set("key", "value3")

	var val string
	if !s.Get("key", &val) {
		t.Fatal("key should exist")
	}
	if val != "value3" {
		t.Errorf("value = %q, want %q", val, "value3")
	}

	// Reload and check the final value
	s2 := New(path)
	s2.Load()
	if !s2.Get("key", &val) {
		t.Fatal("key should exist after reload")
	}
	if val != "value3" {
		t.Errorf("reloaded value = %q, want %q", val, "value3")
	}
}
