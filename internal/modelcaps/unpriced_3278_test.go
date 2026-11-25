package modelcaps

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// tapNoRates records every WARN about listed models with no rates.
func tapNoRates(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	t.Cleanup(log.AddTap(log.WARN, func(level log.Level, _, msg string) {
		if level != log.WARN || !strings.Contains(msg, "no rates") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg)
	}))
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func capsOf(ids ...string) map[string]Caps {
	m := make(map[string]Caps, len(ids))
	for _, id := range ids {
		m[id] = Caps{}
	}
	return m
}

// TestListedModelsWithoutRatesWarnOnce (#3278): a delegated backend's list with
// unpriced models logs one WARN naming all of them and none of the priced ones.
// A later list of this process (another agent's app-server, an upgraded codex)
// names only the models not named yet, and is silent when there are none.
func TestListedModelsWithoutRatesWarnOnce(t *testing.T) {
	reset()
	warns := tapNoRates(t)

	Publish(BackendCodex, capsOf("gpt-6.2-sol", "nr3278-b", "nr3278-a"))
	got := warns()
	if len(got) != 1 {
		t.Fatalf("WARN lines = %q, want one", got)
	}
	if !strings.Contains(got[0], "codex/nr3278-a, codex/nr3278-b") {
		t.Errorf("WARN = %q, want both unpriced models, sorted", got[0])
	}
	if strings.Contains(got[0], "gpt-6.2-sol") {
		t.Errorf("WARN = %q names a priced model", got[0])
	}

	Publish(BackendCodex, capsOf("nr3278-a", "nr3278-b"))
	if got := warns(); len(got) != 1 {
		t.Errorf("WARN lines after the same list again = %q, want still one", got)
	}
	Publish(BackendCodex, capsOf("nr3278-a", "nr3278-c"))
	if got := warns(); len(got) != 2 || !strings.Contains(got[1], "codex/nr3278-c") || strings.Contains(got[1], "nr3278-a") {
		t.Errorf("WARN lines after a new model = %q, want a second naming only nr3278-c", got)
	}
}

// TestListedModelsWarnOnceConcurrently: lists that arrive together (one
// app-server per agent) still log one WARN.
func TestListedModelsWarnOnceConcurrently(t *testing.T) {
	reset()
	warns := tapNoRates(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { Publish(BackendCodex, capsOf("nr3278-a")) })
	}
	wg.Wait()
	if got := warns(); len(got) != 1 {
		t.Errorf("WARN lines = %q, want one", got)
	}
}

// TestPricedListDoesNotSpendTheWarning: an all-priced list is silent, and an
// unpriced model in a later list is still reported.
func TestPricedListDoesNotSpendTheWarning(t *testing.T) {
	reset()
	warns := tapNoRates(t)
	// claude-opus-9-9 has no row and takes its family's rates: priced.
	Publish(BackendCCStream, capsOf("claude-haiku-4-5", "claude-opus-9-9"))
	Publish(BackendCodex, capsOf("gpt-6.2-sol"))
	Publish(BackendCodex, nil)
	if got := warns(); len(got) != 0 {
		t.Fatalf("WARN lines on priced lists = %q, want none", got)
	}
	Publish(BackendCodex, capsOf("gpt-6.2-sol", "nr3278-a"))
	if got := warns(); len(got) != 1 {
		t.Errorf("WARN lines = %q, want one", got)
	}
}

// TestZeroRateAndAPIListsDoNotWarn: a model with a deliberate zero rate is
// priced (its rates gone, the same list warns: the control), and the direct-API
// list is not checked, since only its calls go through [endpoints], where
// zero_cost (#3035) is set.
func TestZeroRateAndAPIListsDoNotWarn(t *testing.T) {
	reset()
	t.Cleanup(modelinfo.ResetToBuiltIn)
	warns := tapNoRates(t)

	modelinfo.Register("", "nr3278-zero", modelinfo.Model{
		Rates: modelinfo.Prices{CacheReadSet: true, CacheWriteSet: true}.Rates(),
	})
	Publish(BackendCodex, capsOf("nr3278-zero"))
	Publish(BackendAPI, capsOf("nr3278-a"))
	if got := warns(); len(got) != 0 {
		t.Fatalf("WARN lines = %q, want none", got)
	}

	modelinfo.ResetToBuiltIn()
	Publish(BackendCodex, capsOf("nr3278-zero"))
	if got := warns(); len(got) != 1 {
		t.Errorf("WARN lines once the zero rate is gone = %q, want one", got)
	}
}

// TestFailedListAndRestoreDoNotWarn: a list that could not be fetched, and the
// persisted list of an earlier process, report nothing. The fetched list that
// follows does.
func TestFailedListAndRestoreDoNotWarn(t *testing.T) {
	reset()
	warns := tapNoRates(t)
	SetPersister(BackendCodex, &fakePersister{loadReturn: capsOf("nr3278-a"), loadAt: time.Now()})
	Restore(BackendCodex)
	if _, ok := LookupFor(BackendCodex, "nr3278-a"); !ok {
		t.Fatal("restore did not seed the cache")
	}
	SetFetcher(BackendCCStream, func(context.Context) (map[string]Caps, error) {
		return capsOf("nr3278-b"), errors.New("offline")
	})
	if err := Refresh(context.Background(), BackendCCStream); err == nil {
		t.Fatal("Refresh = nil, want the fetch error")
	}
	if got := warns(); len(got) != 0 {
		t.Fatalf("WARN lines = %q, want none", got)
	}
	SetFetcher(BackendCCStream, func(context.Context) (map[string]Caps, error) {
		return capsOf("nr3278-b"), nil
	})
	if err := Refresh(context.Background(), BackendCCStream); err != nil {
		t.Fatal(err)
	}
	if got := warns(); len(got) != 1 || !strings.Contains(got[0], "ccstream/nr3278-b") {
		t.Errorf("WARN lines = %q, want one naming ccstream/nr3278-b", got)
	}
}
