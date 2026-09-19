package telemetry

import (
	"context"
	"time"

	"foci/internal/log"
)

// flushForTest forces the current provider to export every buffered span
// synchronously (BatchSpanProcessor.ForceFlush blocks until the export
// completes), so a test can read an in-memory exporter deterministically
// right after driving a scenario, without waiting out batchTimeout. No-op
// when never initialised.
func flushForTest(ctx context.Context) error {
	mu.RLock()
	tp := tracerProvider
	mu.RUnlock()
	if tp == nil {
		return nil
	}
	return tp.ForceFlush(ctx)
}

// resetForTest tears down whatever initWith left behind and clears every
// package-level registry (subagent runs, active/last/pending links, system
// prompt maps) so tests are independent of each other under repeated runs
// (-count=2) regardless of order. Safe to call when never initialised.
func resetForTest() {
	enabled.Store(false)
	log.APIHook = nil
	log.CorrectionHook = nil

	mu.Lock()
	tp := tracerProvider
	tracerProvider = nil
	tracer = nil
	opts = Options{}
	redactor = nil
	mu.Unlock()
	if tp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = tp.Shutdown(ctx)
		cancel()
	}

	subMu.Lock()
	subRuns = map[string]*subagentRun{}
	subMu.Unlock()

	linkMu.Lock()
	active = map[string]string{}
	last = map[string]string{}
	pending = map[string][]Link{}
	linkMu.Unlock()

	spMu.Lock()
	spCurrent = map[string]systemPrompt{}
	spExported = map[string]string{}
	spMu.Unlock()
}
