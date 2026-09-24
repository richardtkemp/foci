package delegator

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/clock"
)

type recLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recLogger) add(level, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+" "+fmt.Sprintf(format, args...))
}
func (r *recLogger) Infof(format string, args ...any)  { r.add("INFO", format, args...) }
func (r *recLogger) Warnf(format string, args ...any)  { r.add("WARN", format, args...) }
func (r *recLogger) Errorf(format string, args ...any) { r.add("ERROR", format, args...) }

func (r *recLogger) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.lines
	r.lines = nil
	return out
}

// A standing violation reports once, then at most once per interval with a
// count of what it held back — and immediately under a version it has not
// reported yet.
func TestExpectationGuard_RateLimit(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake()
	g := &ExpectationGuard{Clock: clk}
	lg := &recLogger{}

	if !g.Violated(lg, "claude-code", "2.1.280", "inv", "first") {
		t.Fatal("first violation was rate-limited")
	}
	lines := lg.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "ERROR BACKEND EXPECTATION VIOLATED: claude-code 2.1.280") {
		t.Fatalf("first report = %q, want one ERROR naming backend and version", lines)
	}

	clk.Advance(ExpectationRepeatInterval / 2)
	for i := 0; i < 3; i++ {
		if g.Violated(lg, "claude-code", "2.1.280", "inv", "again") {
			t.Fatal("repeat within the interval was reported")
		}
	}
	if lines := lg.take(); len(lines) != 0 {
		t.Fatalf("suppressed repeats logged %q", lines)
	}
	// A different invariant is not held back by the first one's limit.
	if !g.Violated(lg, "claude-code", "2.1.280", "other", "x") {
		t.Error("a different invariant was rate-limited by another's report")
	}
	lg.take()

	clk.Advance(ExpectationRepeatInterval)
	if !g.Violated(lg, "claude-code", "2.1.280", "inv", "later") {
		t.Fatal("violation after the interval was not reported")
	}
	if lines := lg.take(); len(lines) != 1 || !strings.Contains(lines[0], "3 more violation(s)") {
		t.Errorf("report after the interval = %q, want the 3 suppressed repeats counted", lines)
	}

	// Immediately, but under a new version: new information, reported.
	if !g.Violated(lg, "claude-code", "2.1.281", "inv", "new version") {
		t.Error("violation under a new backend version was rate-limited")
	}
	if got := g.Count("claude-code", "inv"); got != 6 {
		t.Errorf("Count = %d, want 6 (every violation, reported or not)", got)
	}
}

func TestExpectationGuard_UnknownVersionIsSaid(t *testing.T) {
	t.Parallel()
	g := &ExpectationGuard{}
	lg := &recLogger{}
	g.Violated(lg, "codex", "", "inv", "d")
	if lines := lg.take(); len(lines) != 1 || !strings.Contains(lines[0], "codex version unknown") {
		t.Errorf("report = %q, want it to say the version is unknown", lines)
	}
}

// First version: INFO. A new one: WARN, naming both. A known one: silent.
func TestExpectationGuard_NoteVersion(t *testing.T) {
	t.Parallel()
	g := &ExpectationGuard{}
	lg := &recLogger{}

	g.NoteVersion(lg, "claude-code", "2.1.261")
	if lines := lg.take(); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") || !strings.Contains(lines[0], "2.1.261") {
		t.Fatalf("first version = %q, want one INFO line", lines)
	}
	g.NoteVersion(lg, "claude-code", "2.1.261")
	if lines := lg.take(); len(lines) != 0 {
		t.Fatalf("same version again logged %q", lines)
	}
	g.NoteVersion(lg, "claude-code", "2.1.280")
	lines := lg.take()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "2.1.261 -> 2.1.280") {
		t.Fatalf("version change = %q, want one WARN naming old and new", lines)
	}
	// An old process restarting on the old binary is not a new update.
	g.NoteVersion(lg, "claude-code", "2.1.261")
	if lines := lg.take(); len(lines) != 0 {
		t.Errorf("returning to a known version logged %q", lines)
	}
	// Backends are tracked separately.
	g.NoteVersion(lg, "codex", "0.145.0")
	if lines := lg.take(); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") {
		t.Errorf("first codex version = %q, want INFO", lines)
	}
	g.NoteVersion(lg, "codex", "")
	if lines := lg.take(); len(lines) != 0 {
		t.Errorf("empty version logged %q", lines)
	}
}
