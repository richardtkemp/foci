package session

import (
	"sync"
	"testing"
)

// TestSwapSystemState covers insert-if-absent, compare-and-swap, and that of
// many concurrent swaps from the same old value exactly one wins (#3049).
func TestSwapSystemState(t *testing.T) {
	idx := newTestSessionIndex(t)

	if ok, err := idx.SwapSystemState("k", "", "pending"); err != nil || !ok {
		t.Fatalf("insert-if-absent on a missing key = %v, %v; want true", ok, err)
	}
	if ok, err := idx.SwapSystemState("k", "", "other"); err != nil || ok {
		t.Fatalf("insert-if-absent on a present key = %v, %v; want false", ok, err)
	}
	if ok, err := idx.SwapSystemState("k", "wrong", "x"); err != nil || ok {
		t.Fatalf("swap from a wrong old value = %v, %v; want false", ok, err)
	}
	if v, _ := idx.GetSystemState("k"); v != "pending" {
		t.Fatalf("value after failed swaps = %q; want pending", v)
	}

	var wins sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 16; i++ {
		wins.Add(1)
		go func() {
			defer wins.Done()
			ok, err := idx.SwapSystemState("k", "pending", "delivered")
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wins.Wait()
	if won != 1 {
		t.Fatalf("%d concurrent swaps won; want exactly 1", won)
	}
}

// TestAnyAgentMetadataEquals reports a match on any agent, and none otherwise.
func TestAnyAgentMetadataEquals(t *testing.T) {
	idx := newTestSessionIndex(t)
	if ok, err := idx.AnyAgentMetadataEquals("first_run_completed", "true"); err != nil || ok {
		t.Fatalf("empty index = %v, %v; want false", ok, err)
	}
	_ = idx.SetAgentMetadata("a", "first_run_completed", "false")
	if ok, _ := idx.AnyAgentMetadataEquals("first_run_completed", "true"); ok {
		t.Fatal("matched a different value")
	}
	_ = idx.SetAgentMetadata("b", "first_run_completed", "true")
	if ok, err := idx.AnyAgentMetadataEquals("first_run_completed", "true"); err != nil || !ok {
		t.Fatalf("with one matching agent = %v, %v; want true", ok, err)
	}
}
