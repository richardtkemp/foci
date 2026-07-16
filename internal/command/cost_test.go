package command

import (
	"context"
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

// --- parseCostArgs tests ---

func TestParseCostArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    string
		wantErr bool
		durKind costDurKind
		scopes  []string
		brk     bool
	}{
		{"empty", "", false, durNone, nil, false},
		{"today", "today", false, durToday, nil, false},
		{"24h", "24h", false, durWindow, nil, false},
		{"week", "week", false, durWindow, nil, false},
		{"4h Go duration", "4h", false, durWindow, nil, false},
		{"3 days numeric", "3", false, durWindow, nil, false},
		{"breakdown only", "breakdown", false, durNone, nil, true},
		{"session scope", "session", false, durNone, []string{"session"}, false},
		{"self alias", "self", false, durNone, []string{"session"}, false},
		{"strict-self", "strict-self", false, durNone, []string{"strict-self"}, false},
		{"descendants alias", "forks", false, durNone, []string{"descendants"}, false},
		{"agent scope", "agent", false, durNone, []string{"agent"}, false},
		{"facet type scope", "facet", false, durNone, []string{"facet"}, false},
		{"today+session", "today session", false, durToday, []string{"session"}, false},
		{"24h+agent+breakdown", "24h agent breakdown", false, durWindow, []string{"agent"}, true},
		{"multiple scopes", "session facet", false, durNone, []string{"session", "facet"}, false},
		{"unknown token", "banana", true, 0, nil, false},
		{"two durations", "today 24h", true, 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseCostArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.args, result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.args, err)
			}
			if result.durKind != tc.durKind {
				t.Errorf("durKind = %v, want %v", result.durKind, tc.durKind)
			}
			if result.breakdown != tc.brk {
				t.Errorf("breakdown = %v, want %v", result.breakdown, tc.brk)
			}
			if len(result.scopes) != len(tc.scopes) {
				t.Errorf("scopes = %v, want %v", result.scopes, tc.scopes)
			} else {
				for i, s := range tc.scopes {
					if result.scopes[i] != s {
						t.Errorf("scopes[%d] = %q, want %q", i, result.scopes[i], s)
					}
				}
			}
		})
	}
}

// --- Scope predicate tests ---

func TestScopePredicate_SessionFamily(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	pred, label := scopePredicate([]string{"session"}, root, idx)

	// Family members must match.
	for _, k := range []string{root, "bot/c123/b100", "bot/c123/b200", "bot/ispawn-1"} {
		if !pred(k) {
			t.Errorf("family member %q should match session scope", k)
		}
	}
	// Unrelated session must not match.
	if pred("bot/c999") {
		t.Error("unrelated bot/c999 should not match session scope")
	}
	if !strings.Contains(label, "this session") {
		t.Errorf("label should mention 'this session', got %q", label)
	}
}

func TestScopePredicate_Descendants(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	pred, _ := scopePredicate([]string{"descendants"}, root, idx)

	// Self must NOT match (descendants only).
	if pred(root) {
		t.Errorf("root %q should not match descendants scope", root)
	}
	// Children must match.
	for _, k := range []string{"bot/c123/b100", "bot/c123/b200", "bot/ispawn-1"} {
		if !pred(k) {
			t.Errorf("descendant %q should match descendants scope", k)
		}
	}
}

func TestScopePredicate_StrictSelf(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	pred, _ := scopePredicate([]string{"strict-self"}, root, idx)

	if !pred(root) {
		t.Error("root should match strict-self")
	}
	if pred("bot/c123/b100") {
		t.Error("child should not match strict-self")
	}
}

func TestScopePredicate_Agent(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	pred, _ := scopePredicate([]string{"agent"}, root, idx)

	// All bot/* sessions should match.
	for _, k := range []string{root, "bot/c999", "bot/c123/b100"} {
		if !pred(k) {
			t.Errorf("agent session %q should match", k)
		}
	}
	// Other agent should not match.
	if pred("other/x1") {
		t.Error("other/x1 should not match agent scope")
	}
}

func TestScopePredicate_SessionType(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	pred, _ := scopePredicate([]string{"reflection"}, root, idx)

	if !pred("bot/c123/b100") {
		t.Error("reflection session should match reflection scope")
	}
	if pred(root) {
		t.Error("chat session should not match reflection scope")
	}
}

func TestScopePredicate_Intersection(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)

	// session ∩ reflection = only reflection sessions in this family.
	pred, _ := scopePredicate([]string{"session", "reflection"}, root, idx)

	if pred(root) {
		t.Error("chat root should not match session ∩ reflection")
	}
	if !pred("bot/c123/b100") {
		t.Error("reflection branch should match session ∩ reflection")
	}
	if pred("bot/c999") {
		t.Error("unrelated chat should not match session ∩ reflection")
	}
}

func TestScopePredicate_NilIndex(t *testing.T) {
	pred, _ := scopePredicate([]string{"session"}, "bot/c123", nil)

	if !pred("bot/c123") {
		t.Error("session key should match session scope with nil index")
	}
	if pred("bot/other") {
		t.Error("other session should not match session scope with nil index")
	}
}

// --- CostCommand rendering tests ---

func TestCostSession_CategoryView(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []log.APIEntry{
		{Timestamp: now, Session: "main/i0/0/abc", CostUSD: 0.010, Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 300},
		{Timestamp: now, Session: "main/i0/0/abc", CostUSD: 0.020, Input: 800, Output: 300, CacheRead: 1500, CacheWrite: 0},
		{Timestamp: now, Session: "other/session", CostUSD: 0.500, Input: 5000, Output: 2000, CacheRead: 10000, CacheWrite: 5000},
	})

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "session", SessionKey: "main/i0/0/abc"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "$0.0300") {
		t.Errorf("expected total $0.0300 for this session, got:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "0.500") || strings.Contains(result.Text, "other/session") {
		t.Errorf("should not contain other session's cost:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "2 calls") {
		t.Errorf("expected 2 calls for this session:\n%s", result.Text)
	}
}

func TestCostSession_WithIndex_FamilyDetail(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)
	now := time.Now().UTC()
	path := writeAPILog(t, []log.APIEntry{
		{Timestamp: now, Session: root, CostUSD: 1.00},
		{Timestamp: now, Session: "bot/c123/b100", CostUSD: 0.10},
		{Timestamp: now, Session: "bot/c999", CostUSD: 9.00},
	})

	cc := costCC(path)
	cc.SessionIndex = idx

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "session", SessionKey: root}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should include family members (1.00 + 0.10 = 1.10) but not unrelated (9.00).
	if !strings.Contains(result.Text, "$1.1000") {
		t.Errorf("expected family total $1.1000, got:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "9.00") {
		t.Errorf("unrelated cost leaked into session family view:\n%s", result.Text)
	}
}

func TestCostSessionBreakdown_WithIndex(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)
	now := time.Now().UTC()
	path := writeAPILog(t, []log.APIEntry{
		{Timestamp: now, Session: root, CostUSD: 1.00},
		{Timestamp: now, Session: root, CostUSD: 1.00},
		{Timestamp: now, Session: "bot/c123/b100", CostUSD: 0.10},
		{Timestamp: now, Session: "bot/c123/b200", CostUSD: 0.05},
		{Timestamp: now, Session: "bot/ispawn-1", CostUSD: 0.50},
		{Timestamp: now, Session: "bot/c999", CostUSD: 9.00},
	})

	cc := costCC(path)
	cc.SessionIndex = idx

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "session breakdown", SessionKey: root}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Family total = 2.00 + 0.10 + 0.05 + 0.50 = 2.65
	if !strings.Contains(result.Text, "$2.65") {
		t.Errorf("expected family total $2.65 in output:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "9.00") {
		t.Errorf("unrelated cost leaked into family breakdown:\n%s", result.Text)
	}
	for _, typ := range []string{"chat", "reflection", "keepalive", "spawn"} {
		if !strings.Contains(result.Text, typ) {
			t.Errorf("breakdown missing type %q:\n%s", typ, result.Text)
		}
	}
	if !strings.Contains(result.Text, "Started 2026-07-1") {
		t.Errorf("expected family start line, got:\n%s", result.Text)
	}
}

func TestCostSession_IncludesStartTime(t *testing.T) {
	idx := costTestIndex(t)
	root, _ := seedFamily(t, idx)
	now := time.Now().UTC()
	path := writeAPILog(t, []log.APIEntry{{Timestamp: now, Session: root, CostUSD: 1.00}})

	cc := costCC(path)
	cc.SessionIndex = idx

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "session", SessionKey: root}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Started 2026-07-11") {
		t.Errorf("session view should show start time, got:\n%s", result.Text)
	}
}
