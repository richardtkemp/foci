package anthropic

import (
	"strings"
	"sync"
	"testing"
)

func TestTokenHolder_GetSet(t *testing.T) {
	// Proves NewTokenHolder initialises with the supplied token and that
	// Set correctly replaces it, with Get returning the updated value.
	h := NewTokenHolder("initial")
	tok, err := h.Get()
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if tok != "initial" {
		t.Errorf("Get = %q, want %q", tok, "initial")
	}

	h.Set("updated")
	tok, err = h.Get()
	if err != nil {
		t.Fatalf("Get after Set: unexpected error: %v", err)
	}
	if tok != "updated" {
		t.Errorf("Get after Set = %q, want %q", tok, "updated")
	}
}

func TestTokenHolder_EmptyReturnsError(t *testing.T) {
	// Proves that Get returns an error when the holder was initialised with
	// an empty string — indicating no credential is configured.
	h := NewTokenHolder("")
	_, err := h.Get()
	if err == nil {
		t.Fatal("expected error for empty tokenHolder")
	}
}

func TestTokenHolder_ConcurrentAccess(t *testing.T) {
	// Proves the RWMutex in tokenHolder prevents data races: concurrent
	// writers and readers must not corrupt the stored token.
	h := NewTokenHolder("start")
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Set("token-" + strings.Repeat("x", i))
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := h.Get()
			if err != nil {
				t.Errorf("concurrent Get error: %v", err)
			}
			if tok == "" {
				t.Error("concurrent Get returned empty")
			}
		}()
	}

	wg.Wait()
}
