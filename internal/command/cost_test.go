package command

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/session"
)

func costTestIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// seedFamily populates a chat root plus a reflection branch, a keepalive
// branch, and a cross-chat independent spawn parented to the root — plus an
// unrelated chat that must NOT be pulled into the family.
func seedFamily(t *testing.T, idx *session.SessionIndex) (root string, earliest time.Time) {
	t.Helper()
	base := time.Date(2026, 7, 11, 7, 14, 0, 0, time.UTC)
	root = "bot/c123"
	earliest = base.Add(-2 * time.Hour) // the spawn is oldest
	idx.Upsert(session.SessionIndexEntry{SessionKey: root, CreatedAt: base, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	idx.Upsert(session.SessionIndexEntry{SessionKey: "bot/c123/b100", CreatedAt: base.Add(time.Hour), ParentSessionKey: root, SessionType: session.SessionTypeReflection, Status: session.SessionStatusActive})
	idx.Upsert(session.SessionIndexEntry{SessionKey: "bot/c123/b200", CreatedAt: base.Add(2 * time.Hour), ParentSessionKey: root, SessionType: session.SessionTypeKeepalive, Status: session.SessionStatusActive})
	// Cross-chat independent spawn: parent is the root, but its own key does NOT
	// share the c123 prefix — only transitive-closure resolution catches it.
	idx.Upsert(session.SessionIndexEntry{SessionKey: "bot/ispawn-1", CreatedAt: earliest, ParentSessionKey: root, SessionType: session.SessionTypeSpawn, Status: session.SessionStatusActive})
	// Unrelated chat — must be excluded from the family.
	idx.Upsert(session.SessionIndexEntry{SessionKey: "bot/c999", CreatedAt: base, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	return root, earliest
}

func TestMoneyCol_Aligns(t *testing.T) {
	cells := moneyCol([]float64{113.9299, 0.0474}, 4)
	if cells[0] != "`$113.9299`" {
		t.Errorf("cells[0] = %q, want `$113.9299`", cells[0])
	}
	if cells[1] != "`$  0.0474`" {
		t.Errorf("cells[1] = %q, want `$  0.0474` (padded to align)", cells[1])
	}
}

func TestBreakdownRequested(t *testing.T) {
	for _, tc := range []struct {
		args string
		want bool
	}{
		{"", false},
		{"breakdown", true},
		{"BreakDown", true},
		{"7 breakdown", true},
		{"7", false},
		{"break", false},
	} {
		if got := breakdownRequested(tc.args); got != tc.want {
			t.Errorf("breakdownRequested(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestSessionFamily_TransitiveClosure(t *testing.T) {
	idx := costTestIndex(t)
	root, earliest := seedFamily(t, idx)

	// Start from a branch, not the root: must still resolve the whole family.
	family, start := sessionFamily(idx, "bot/c123/b100")

	want := []string{root, "bot/c123/b100", "bot/c123/b200", "bot/ispawn-1"}
	for _, k := range want {
		if _, ok := family[k]; !ok {
			t.Errorf("family missing %q", k)
		}
	}
	if _, ok := family["bot/c999"]; ok {
		t.Error("family wrongly included unrelated chat bot/c999")
	}
	if !start.Equal(earliest) {
		t.Errorf("family start = %v, want earliest %v", start, earliest)
	}
}

func TestCostSessionBreakdown_ByType(t *testing.T) {
	idx := costTestIndex(t)
	seedFamily(t, idx)

	entries := []log.APIEntry{
		{Session: "bot/c123", CostUSD: 1.00},
		{Session: "bot/c123", CostUSD: 1.00},
		{Session: "bot/c123/b100", CostUSD: 0.10},
		{Session: "bot/c123/b200", CostUSD: 0.05},
		{Session: "bot/ispawn-1", CostUSD: 0.50},
		{Session: "bot/c999", CostUSD: 9.00}, // unrelated — must be excluded
	}

	out := costSessionBreakdown(entries, "bot/c123", idx)

	// Family total is 2.00+0.10+0.05+0.50 = 2.65, excluding the 9.00 unrelated.
	if !strings.Contains(out, "$2.65") {
		t.Errorf("expected family total $2.65 in output:\n%s", out)
	}
	if strings.Contains(out, "9.00") {
		t.Errorf("unrelated chat cost leaked into family breakdown:\n%s", out)
	}
	for _, typ := range []string{"chat", "reflection", "keepalive", "spawn"} {
		if !strings.Contains(out, typ) {
			t.Errorf("breakdown missing type %q:\n%s", typ, out)
		}
	}
	if !strings.Contains(out, "Started 2026-07-1") {
		t.Errorf("expected family start line, got:\n%s", out)
	}
}

func TestRenderTypeBreakdown_UntypedBucket(t *testing.T) {
	typeMap := map[string]string{"bot/c123": "chat"}
	entries := []log.APIEntry{
		{Session: "bot/c123", CostUSD: 1.00},
		{Session: "bot/cGHOST", CostUSD: 0.25}, // absent from index
	}
	out := renderTypeBreakdown(entries, typeMap, "Test")
	if !strings.Contains(out, "(untyped)") {
		t.Errorf("expected (untyped) bucket for unindexed key:\n%s", out)
	}
	if !strings.Contains(out, "$1.25") {
		t.Errorf("expected total $1.25:\n%s", out)
	}
}

func TestCostSession_IncludesStartTime(t *testing.T) {
	idx := costTestIndex(t)
	seedFamily(t, idx)
	entries := []log.APIEntry{{Session: "bot/c123", CostUSD: 1.00}}
	out := costSession(entries, "bot/c123", idx, false)
	if !strings.Contains(out, "Started 2026-07-11") {
		t.Errorf("plain /cost session should show start time, got:\n%s", out)
	}
}
