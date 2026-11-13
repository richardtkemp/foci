package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/memory"
)

// TestRemindWake_ScheduleFailureDismissFailureLogged pins #3080 for the remind
// tool: when scheduling a stored wake fails, the tool deletes the row; if that
// delete fails too, the never-armed wake would fire at the next start, so the
// failure is logged at ERROR. The schedule fn closes the store before it
// fails, so the cleanup Dismiss fails.
func TestRemindWake_ScheduleFailureDismissFailureLogged(t *testing.T) {
	var mu sync.Mutex
	var errs []string
	remove := log.AddTap(log.ERROR, func(_ log.Level, component, msg string) {
		if component != "remind" {
			return
		}
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	})
	defer remove()

	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	failing := func(int64, time.Duration, string, string) error {
		if err := rs.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		return errors.New("scheduler down")
	}
	tool := NewRemindTool(rs, "test", failing, nil)
	params, _ := json.Marshal(map[string]interface{}{"text": "never armed", "when": "1h", "wake": true})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("Execute succeeded despite a failing schedule fn")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range errs {
		if strings.Contains(m, "dismiss after schedule failure failed") {
			return
		}
	}
	t.Fatalf("no ERROR line for the failed cleanup dismiss; remind errors: %v", errs)
}
