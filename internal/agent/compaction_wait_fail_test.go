package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// newFailingCompactAgent builds a delegated agent whose backend accepts
// /compact (or refuses it with injectErr) and whose wait for the compaction
// returns waitErr. It records every start and notify message.
func newFailingCompactAgent(t *testing.T, sessionKey string, injectErr, waitErr error) (ag *Agent, starts, notifies *[]string) {
	t.Helper()
	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true, waitForTurnErr: waitErr}
			be.sendCommandFn = func(context.Context, string) error { return injectErr }
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })
	if _, err := dm.Get(context.Background(), sessionKey); err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}
	ag = &Agent{
		Sessions:         session.NewStore(t.TempDir()),
		Bootstrap:        workspace.NewBootstrap(t.TempDir(), []string{}),
		DelegatedManager: dm,
	}
	var s, n []string
	ag.CompactionStartFunc.Add(func(_, msg string) { s = append(s, msg) })
	ag.CompactionNotifyFunc.Add(func(_, msg, _ string) { n = append(n, msg) })
	return ag, &s, &n
}

// TestRunDelegatedCompact_FailedWaitResolvesNotice is the regression test for
// #2947: when the wait for a delegated compaction fails, the ⏳ "Compacting
// context..." notice already sent must be resolved (edited to ❌ through the
// notify hook), not left showing "in progress" forever.
func TestRunDelegatedCompact_FailedWaitResolvesNotice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		waitErr error
		want    string // substring the ❌ notice must carry
	}{
		{name: "timeout", waitErr: context.DeadlineExceeded, want: "foci stopped waiting"},
		{name: "backend error", waitErr: errors.New("codex: process exited"), want: "codex: process exited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag, starts, notifies := newFailingCompactAgent(t, "test/ifailwait-"+tc.name, nil, tc.waitErr)
			_, err := ag.CompactSession(context.Background(), "test/ifailwait-"+tc.name, false)
			if err == nil {
				t.Fatal("CompactSession: want the wait error, got nil")
			}
			if len(*starts) != 1 {
				t.Fatalf("start notices = %q, want one ⏳", *starts)
			}
			if len(*notifies) != 1 {
				t.Fatalf("notify messages = %q, want exactly one resolving the ⏳", *notifies)
			}
			got := (*notifies)[0]
			if !strings.HasPrefix(got, "❌") || !strings.Contains(got, tc.want) {
				t.Errorf("notify = %q, want a ❌ notice containing %q", got, tc.want)
			}
		})
	}
}

// TestRunDelegatedCompact_InjectFailureSendsNoNotice: when /compact cannot even
// be sent, no ⏳ was shown, so there is nothing to resolve and no ❌ may appear
// as a stray message.
func TestRunDelegatedCompact_InjectFailureSendsNoNotice(t *testing.T) {
	t.Parallel()
	ag, starts, notifies := newFailingCompactAgent(t, "test/ifailinject", errors.New("pipe closed"), nil)
	if _, err := ag.CompactSession(context.Background(), "test/ifailinject", false); err == nil {
		t.Fatal("CompactSession: want the send error, got nil")
	}
	if len(*starts) != 0 || len(*notifies) != 0 {
		t.Errorf("starts=%q notifies=%q, want none", *starts, *notifies)
	}
}

// TestDelegatedCompactTimeout_OutlastsObservedCodexCompaction pins the wait
// budget above a real codex compaction: on the work Mac (2026-11-07) a
// 379k-token context took 5m18s to compact, and the old 5-minute budget
// reported that success as a failure (#2947).
func TestDelegatedCompactTimeout_OutlastsObservedCodexCompaction(t *testing.T) {
	t.Parallel()
	const observed = 5*time.Minute + 18*time.Second
	if delegatedCompactTimeout < 3*observed {
		t.Errorf("delegatedCompactTimeout = %v, want at least 3× the observed %v codex compaction", delegatedCompactTimeout, observed)
	}
}

// TestRunDelegatedCompact_DeclinedResolvesNotice: a backend that declines to
// compact (ErrCompactionNoBoundary) also leaves a ⏳ behind; it resolves to
// the "nothing to compact" text instead of staying "in progress" (#2947).
func TestRunDelegatedCompact_DeclinedResolvesNotice(t *testing.T) {
	t.Parallel()
	ag, _, notifies := newFailingCompactAgent(t, "test/ideclined", nil, delegator.ErrCompactionNoBoundary)
	_, err := ag.CompactSession(context.Background(), "test/ideclined", false)
	if !errors.Is(err, delegator.ErrCompactionNoBoundary) {
		t.Fatalf("CompactSession: err = %v, want ErrCompactionNoBoundary", err)
	}
	if len(*notifies) != 1 || !strings.Contains((*notifies)[0], "Nothing to compact") {
		t.Errorf("notify messages = %q, want one 'Nothing to compact' notice", *notifies)
	}
}

// TestDoCompact_FailureResolvesNotice: an API-transport compaction (doCompact)
// that fails after its ⏳ was sent must resolve that notice to ❌, dry-run
// included, and mark the error as told by the notice (#2947).
func TestDoCompact_FailureResolvesNotice(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
			t.Parallel()
			store := session.NewStore(t.TempDir())
			sk := fmt.Sprintf("test/idofail-%v", dryRun)
			seedSession(t, store, sk, 3)
			ag := &Agent{
				Client: &testClient{handler: func(context.Context, *provider.MessageRequest) (*provider.MessageResponse, error) {
					return nil, errors.New("upstream 529 overloaded")
				}},
				Sessions:  store,
				Tools:     tools.NewRegistry(),
				Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
				Compactor: compaction.NewCompactor(store, 0.8),
				Model:     "claude-haiku-4-5",
			}
			var starts, notifies []string
			ag.CompactionStartFunc.Add(func(_, msg string) { starts = append(starts, msg) })
			ag.CompactionNotifyFunc.Add(func(_, msg, _ string) { notifies = append(notifies, msg) })

			_, err := ag.CompactSession(context.Background(), sk, dryRun)
			if err == nil {
				t.Fatal("CompactSession: want the compaction error, got nil")
			}
			if !CompactionNoticeShown(err) {
				t.Errorf("err = %v, want it marked CompactionNoticeShown", err)
			}
			if len(starts) != 1 {
				t.Fatalf("start notices = %q, want one ⏳", starts)
			}
			if len(notifies) != 1 || !strings.HasPrefix(notifies[0], "❌") || !strings.Contains(notifies[0], "overloaded") {
				t.Errorf("notify messages = %q, want one ❌ naming the error", notifies)
			}
		})
	}
}
