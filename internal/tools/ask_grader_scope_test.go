package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for #3083: an ask grader may run only from the agent's skill
// directories, its workspace, /tmp and tools.file_allowed_paths. Every
// writable test dir lies under a temp area, so these tests remove the
// built-in temp areas (withTempRoots()) to get an "outside" dir.

type graderScopeFixture struct {
	ws, skill, outside, extra string
	skills, live              *liveRoots
	tool                      *Tool
	p                         *fakePresenter
	d                         *fakeDeliver
}

func newGraderScopeFixture(t *testing.T) *graderScopeFixture {
	t.Helper()
	base := t.TempDir()
	f := &graderScopeFixture{
		ws:      filepath.Join(base, "ws"),
		skill:   filepath.Join(base, "skills"),
		outside: filepath.Join(base, "outside"),
		extra:   filepath.Join(base, "extra"),
		skills:  &liveRoots{},
		live:    &liveRoots{},
		p:       &fakePresenter{},
		d:       &fakeDeliver{},
	}
	for _, d := range []string{f.ws, f.skill, f.outside, f.extra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.skills.set(f.skill)
	f.tool, _ = NewAskTool(f.p.present, nil, f.d.deliver, nil, nil, "test",
		WithGraderAreas(f.skills.get, WithWorkspace(f.ws), WithExtraRoots(f.live.get), withTempRoots()))
	return f
}

// grader writes an executable grader in dir that prints GRADED-<name>.
func (f *graderScopeFixture) grader(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\necho GRADED-"+name+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func graderAsk(grader string) json.RawMessage {
	return json.RawMessage(`{"questions":[{"question":"color?","header":"Color","options":[{"label":"Red"}]}],"grader":"` + grader + `"}`)
}

func TestAskGraderScope_AllowedAreasRun(t *testing.T) {
	t.Parallel()
	for _, area := range []string{"skill", "workspace", "allowed"} {
		t.Run(area, func(t *testing.T) {
			t.Parallel()
			f := newGraderScopeFixture(t) // one ask per fixture: a second would queue
			f.live.set(f.extra)
			dir := map[string]string{"skill": f.skill, "workspace": f.ws, "allowed": f.extra}[area]
			g := f.grader(t, dir, area)
			if _, err := f.tool.Execute(askCtx(), graderAsk(g)); err != nil {
				t.Fatalf("grader refused: %v", err)
			}
			f.p.answer("qa:0")
			if msg := waitDeliver(t, f.d); !strings.Contains(msg, "GRADED-"+area) {
				t.Errorf("grader did not run, got: %q", msg)
			}
		})
	}
}

func TestAskGraderScope_OutsideRefusedAtCallTime(t *testing.T) {
	t.Parallel()
	f := newGraderScopeFixture(t)
	g := f.grader(t, f.outside, "out")
	_, err := f.tool.Execute(askCtx(), graderAsk(g))
	if err == nil {
		t.Fatal("grader outside the allowed areas was accepted")
	}
	if !strings.Contains(err.Error(), "outside the directories a grader may run from") ||
		!strings.Contains(err.Error(), FileAllowedPathsKey) {
		t.Errorf("refusal should name the rule and the config key, got: %v", err)
	}
	f.p.mu.Lock()
	presents := f.p.presents
	f.p.mu.Unlock()
	if presents != 0 {
		t.Error("a refused ask must not be presented")
	}
}

// A symlink in an allowed area to an executable outside it is judged by its
// target.
func TestAskGraderScope_SymlinkOutRefused(t *testing.T) {
	t.Parallel()
	f := newGraderScopeFixture(t)
	target := f.grader(t, f.outside, "real")
	link := filepath.Join(f.skill, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := f.tool.Execute(askCtx(), graderAsk(link))
	if err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("symlink out of the skill dir should be refused naming its target, got: %v", err)
	}
}

// The areas are live: a grader accepted at call time is re-checked when it
// is about to run, and the user's answers are still delivered.
func TestAskGraderScope_RecheckedAtRunTime(t *testing.T) {
	t.Parallel()
	f := newGraderScopeFixture(t)
	f.live.set(f.extra)
	g := f.grader(t, f.extra, "later")
	if _, err := f.tool.Execute(askCtx(), graderAsk(g)); err != nil {
		t.Fatalf("grader refused at call time: %v", err)
	}
	f.live.set() // the operator removes the directory before the user answers
	f.p.answer("qa:0")
	msg := waitDeliver(t, f.d)
	if strings.Contains(msg, "GRADED-later") {
		t.Fatalf("grader ran after its directory was removed from the allowed areas: %q", msg)
	}
	if !strings.Contains(msg, "grader could not run") || !strings.Contains(msg, "Red") {
		t.Errorf("expected the raw answers with a grader-failure note, got: %q", msg)
	}
}

// A live skill-dir change applies on the next call.
func TestAskGraderScope_SkillDirsLive(t *testing.T) {
	t.Parallel()
	f := newGraderScopeFixture(t)
	g := f.grader(t, f.outside, "late-skill")
	if _, err := f.tool.Execute(askCtx(), graderAsk(g)); err == nil {
		t.Fatal("grader outside every area was accepted")
	}
	f.skills.set(f.skill, f.outside)
	if _, err := f.tool.Execute(askCtx(), graderAsk(g)); err != nil {
		t.Fatalf("grader in a newly listed skill dir refused: %v", err)
	}
}
