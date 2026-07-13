package config

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestLiveValueLoadStore(t *testing.T) {
	lv := NewLiveValue(7)
	if got := lv.Load(); got != 7 {
		t.Fatalf("Load = %d, want 7", got)
	}
	lv.Store(42)
	if got := lv.Load(); got != 42 {
		t.Fatalf("Load after Store = %d, want 42", got)
	}
}

func TestLiveValueOnChangeSeesNewValue(t *testing.T) {
	lv := NewLiveValue("a")
	var gotOld, gotNew, loadedInCallback string
	lv.OnChange(func(old, new string) {
		gotOld, gotNew = old, new
		loadedInCallback = lv.Load() // must already reflect the swap
	})
	lv.Store("b")
	if gotOld != "a" || gotNew != "b" {
		t.Errorf("callback got (%q,%q), want (a,b)", gotOld, gotNew)
	}
	if loadedInCallback != "b" {
		t.Errorf("Load inside callback = %q, want b (swap happens before notify)", loadedInCallback)
	}
}

func TestLiveValueOnChangeOrderAndMultiple(t *testing.T) {
	lv := NewLiveValue(0)
	var order []int
	lv.OnChange(func(_, _ int) { order = append(order, 1) })
	lv.OnChange(func(_, _ int) { order = append(order, 2) })
	lv.OnChange(func(_, _ int) { order = append(order, 3) })
	lv.Store(1)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("callback order = %v, want [1 2 3]", order)
	}
}

func TestLiveValueNoSubsIsFine(t *testing.T) {
	lv := NewLiveValue(struct{ X int }{X: 1})
	lv.Store(struct{ X int }{X: 2}) // must not panic with zero subscribers
	if lv.Load().X != 2 {
		t.Errorf("X = %d, want 2", lv.Load().X)
	}
}

// TestLiveValueConcurrent exercises the race detector: many readers Loading
// while a writer Stores, plus a subscriber counting notifications.
func TestLiveValueConcurrent(t *testing.T) {
	lv := NewLiveValue(0)
	var notifies atomic.Int64
	lv.OnChange(func(_, _ int) { notifies.Add(1) })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = lv.Load()
			}
		}()
	}
	const stores = 500
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 1; j <= stores; j++ {
			lv.Store(j)
		}
	}()
	wg.Wait()

	if lv.Load() != stores {
		t.Errorf("final Load = %d, want %d", lv.Load(), stores)
	}
	if got := notifies.Load(); got != stores {
		t.Errorf("notifications = %d, want %d", got, stores)
	}
}
