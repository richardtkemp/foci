package main

// #3210: /agents delete deletes an agent inside the running gateway after two
// confirmations. These tests drive liveAgentStarter.Delete on the rename
// fixture (real registry, fake teardown and disk), and deleteFlow with a fake
// deleter and presenter, so they check the orchestration and that nothing
// happens without both confirmations. The disk delete has its own tests in
// internal/agentrename.

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agentrename"
	"foci/internal/command"
)

// withDeleteDisk gives the rename fixture a fake disk delete that removes the
// id from the fake foci.toml (after planFail / runFail checks).
func withDeleteDisk(f *renameFixture) {
	f.starter.deleteDisk = func(_ context.Context, o agentrename.DeleteOptions) (agentrename.DeleteResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		res := agentrename.DeleteResult{Summary: []string{"its workspace"}, Backup: "/b"}
		if o.PlanOnly {
			return res, f.planFail
		}
		f.diskRuns++
		if f.runFail != nil && !f.failAfter {
			return res, f.runFail
		}
		if i := slices.Index(f.cfgIDs, o.ID); i >= 0 {
			f.cfgIDs = slices.Delete(f.cfgIDs, i, i+1)
		}
		if f.runFail != nil {
			return res, f.runFail
		}
		_, _ = io.WriteString(o.Out, "Checklist:\n  - Backup: /b\n")
		return res, nil
	}
}

func TestLiveDelete_StopsOnlyThatAgentAndStartsNothing(t *testing.T) {
	f := newRenameFixture(t)
	withDeleteDisk(f)
	if _, err := f.starter.Delete(context.Background(), "bob", io.Discard); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !slices.Equal(f.stopped, []string{"bob"}) {
		t.Errorf("stopped %v, want only bob", f.stopped)
	}
	if got := f.order(); !slices.Equal(got, []string{testAgentID, "carol"}) {
		t.Errorf("agent order = %v", got)
	}
	if _, ok := f.d.agents.Get("bob"); ok {
		t.Error("bob still runs")
	}
}

func TestLiveDelete_RefusedPlanStopsNothing(t *testing.T) {
	f := newRenameFixture(t)
	withDeleteDisk(f)
	f.planFail = errors.New(`"bob" is the only agent`)
	if _, err := f.starter.Delete(context.Background(), "bob", io.Discard); err == nil {
		t.Fatal("a refused plan reported success")
	}
	if len(f.stopped) != 0 || f.diskRuns != 0 {
		t.Errorf("a refused plan stopped %v and ran the disk delete %d time(s)", f.stopped, f.diskRuns)
	}
	if _, ok := f.d.agents.Get("bob"); !ok {
		t.Error("bob is not running after a refused plan")
	}
}

func TestLiveDelete_FailureBeforeConfigRestartsTheAgent(t *testing.T) {
	f := newRenameFixture(t)
	withDeleteDisk(f)
	f.runFail = errors.New("backup: disk full")
	_, err := f.starter.Delete(context.Background(), "bob", io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"bob" runs again`) || !strings.Contains(err.Error(), "/agents delete bob") {
		t.Fatalf("err = %v", err)
	}
	if got := f.order(); !slices.Equal(got, []string{testAgentID, "bob", "carol"}) {
		t.Errorf("agent order = %v, want bob back in its place", got)
	}
}

func TestLiveDelete_FailureAfterConfigSaysHowToFinish(t *testing.T) {
	f := newRenameFixture(t)
	withDeleteDisk(f)
	f.runFail, f.failAfter = errors.New("workspace: permission denied"), true
	_, err := f.starter.Delete(context.Background(), "bob", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no longer names") || !strings.Contains(err.Error(), "/agents delete bob") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.d.agents.Get("bob"); ok {
		t.Error("bob was started although foci.toml no longer names it")
	}
}

// fakeDeleter records the flow's calls.
type fakeDeleter struct {
	mu       sync.Mutex
	planErr  error
	deleted  []string
	deleteCh chan struct{}
}

func (d *fakeDeleter) PlanDelete(_ context.Context, id string) (agentrename.DeleteResult, error) {
	return agentrename.DeleteResult{Summary: []string{"its workspace /w/" + id + " (to the trash)"}, Backup: "/b"}, d.planErr
}

func (d *fakeDeleter) Delete(_ context.Context, id string, _ io.Writer) (agentrename.DeleteResult, error) {
	d.mu.Lock()
	d.deleted = append(d.deleted, id)
	d.mu.Unlock()
	close(d.deleteCh)
	return agentrename.DeleteResult{Summary: []string{"its workspace"}}, nil
}

// fakePresenter records each prompt and answers it with the next scripted
// choice (synchronously, as a tap would arrive later on another goroutine).
type fakePresenter struct {
	prompts []deletePrompt
	answers []string
	fail    error
}

func (p *fakePresenter) present(pr deletePrompt, answer func(string)) error {
	if p.fail != nil {
		return p.fail
	}
	p.prompts = append(p.prompts, pr)
	if len(p.answers) > 0 {
		a := p.answers[0]
		p.answers = p.answers[1:]
		answer(a)
	}
	return nil
}

func TestDeleteFlow_RunsOnlyAfterBothConfirmations(t *testing.T) {
	d := &fakeDeleter{deleteCh: make(chan struct{})}
	p := &fakePresenter{answers: []string{deleteConfirmed, deleteConfirmed}}
	var mu sync.Mutex
	var notes []string
	notify := func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() }
	reply := deleteFlow(context.Background(), d, command.AgentDeleteArgs{ID: "bob", Session: "clutch/c1", Source: "app"}, p.present, notify)
	if !strings.Contains(reply, "confirm it twice") {
		t.Errorf("reply = %q", reply)
	}
	select {
	case <-d.deleteCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the delete did not run after both confirmations")
	}
	if len(p.prompts) != 2 {
		t.Fatalf("prompts = %d, want 2", len(p.prompts))
	}
	first, second := p.prompts[0], p.prompts[1]
	for i, pr := range p.prompts {
		if !strings.Contains(pr.Text, `"bob"`) || !strings.Contains(pr.Text, "/w/bob (to the trash)") {
			t.Errorf("prompt %d does not name the agent and what goes: %q", i+1, pr.Text)
		}
	}
	if first.Warning != "" || second.Warning != "DELETE AGENT BOB" {
		t.Errorf("warnings = %q, %q; want only the second big and red", first.Warning, second.Warning)
	}
	if first.ID == second.ID || second.Confirm != "Delete bob" {
		t.Errorf("prompt ids %q %q, second button %q", first.ID, second.ID, second.Confirm)
	}
}

func TestDeleteFlow_CancelOrRefusalChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  []string
		planErr  error
		showErr  error
		prompts  int
		replyHas string
	}{
		{"cancel first", []string{deleteCancelled}, nil, nil, 1, "confirm it twice"},
		{"cancel second", []string{deleteConfirmed, deleteCancelled}, nil, nil, 2, "confirm it twice"},
		{"no answer yet", nil, nil, nil, 1, "confirm it twice"},
		{"plan refused", nil, errors.New("the last agent cannot be deleted"), nil, 0, "refused, nothing changed"},
		{"prompt not shown", nil, nil, errors.New("no foci app device"), 0, "not started, nothing changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDeleter{deleteCh: make(chan struct{}), planErr: tc.planErr}
			p := &fakePresenter{answers: tc.answers, fail: tc.showErr}
			reply := deleteFlow(context.Background(), d, command.AgentDeleteArgs{ID: "bob", Session: "clutch/c1"}, p.present, func(string) {})
			if !strings.Contains(reply, tc.replyHas) {
				t.Errorf("reply = %q, want %q", reply, tc.replyHas)
			}
			if len(p.prompts) != tc.prompts {
				t.Errorf("prompts = %d, want %d", len(p.prompts), tc.prompts)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.deleted) != 0 {
				t.Errorf("deleted %v without two confirmations", d.deleted)
			}
		})
	}
}
