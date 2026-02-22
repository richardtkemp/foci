package agent

import (
	"fmt"
	"sync"
)

// WarningQueue collects log warnings and errors for injection into agent turns.
// Thread-safe: warnings may be pushed from any goroutine and are drained
// by the agent loop before each turn.
type WarningQueue struct {
	mu       sync.Mutex
	warnings []string
	maxSize  int // drop oldest if exceeded (default 50)
}

// NewWarningQueue creates a warning queue.
func NewWarningQueue() *WarningQueue {
	return &WarningQueue{maxSize: 50}
}

// Push adds a warning to the queue.
func (q *WarningQueue) Push(level, component, msg string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry := fmt.Sprintf("[%s] [%s] %s", level, component, msg)
	q.warnings = append(q.warnings, entry)

	// Drop oldest if we exceed max size
	if len(q.warnings) > q.maxSize {
		q.warnings = q.warnings[len(q.warnings)-q.maxSize:]
	}
}

// Drain returns all queued warnings and clears the queue.
func (q *WarningQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.warnings) == 0 {
		return nil
	}
	result := q.warnings
	q.warnings = nil
	return result
}

// Len returns the number of queued warnings.
func (q *WarningQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.warnings)
}
