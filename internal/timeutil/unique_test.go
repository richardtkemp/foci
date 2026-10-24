package timeutil

import (
	"sync"
	"testing"
	"time"
)

func TestUniqueNanoStrictlyIncreasingUnderConcurrency(t *testing.T) {
	const goroutines, each = 8, 5000
	var mu sync.Mutex
	seen := make(map[int64]bool, goroutines*each)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, each)
			for i := range local {
				local[i] = UniqueNano()
				if i > 0 && local[i] <= local[i-1] {
					t.Errorf("not strictly increasing: %d then %d", local[i-1], local[i])
					return
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, v := range local {
				if seen[v] {
					t.Errorf("duplicate value %d", v)
					return
				}
				seen[v] = true
			}
		}()
	}
	wg.Wait()
}

func TestUniqueNanoTracksWallClock(t *testing.T) {
	before := time.Now().UnixNano()
	got := UniqueNano()
	// Bumps add at most one per call made so far in this process; the value
	// must still be a timestamp near now, not a counter.
	if d := time.Duration(got - before); d < 0 || d > time.Second {
		t.Errorf("UniqueNano = %d, %v from now: want a current timestamp", got, d)
	}
}
