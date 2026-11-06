package ccstream

import (
	"context"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// TestStart_ProcessDeadAtOnce starts backends whose process exits at once. The
// reader can then reach finalizeExit before Start has returned, and finalizeExit
// reads exitCh. Start used to create exitCh after it started the reader, so that
// read could see nil, skip the wait for the reap, and lose the exit detail
// (#2883). The fail is a -race report and a plain run cannot see the ordering.
// Concurrent starts load the scheduler, which opens the window: about 1% of runs
// caught it on the unfixed code, so this guards the ordering only weakly.
func TestStart_ProcessDeadAtOnce(t *testing.T) {
	const starters, rounds = 16, 5
	var wg sync.WaitGroup
	for range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				be, err := newFromConfig(map[string]any{"binary": "/usr/bin/true"})
				if err != nil {
					t.Errorf("newFromConfig: %v", err)
					return
				}
				b := be.(*Backend)
				if err := b.Start(context.Background(), delegator.StartOptions{
					WorkDir: t.TempDir(), AgentID: "exitchtest",
				}); err != nil {
					t.Errorf("Start: %v", err)
					return
				}
				_ = b.Close()
			}
		}()
	}
	wg.Wait()
}
