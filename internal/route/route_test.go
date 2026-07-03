package route

import (
	"errors"
	"testing"

	"foci/internal/session"
)

// TestParseTarget proves the canonical target grammar parses agent, rest, and
// params, applies defaults (create=true, policy=fallback), and rejects
// malformed inputs.
func TestParseTarget(t *testing.T) {
	cases := []struct {
		in   string
		want Target
	}{
		{"clutch", Target{Agent: "clutch", Create: true, Policy: PolicyFallback}},
		{"clutch/c123", Target{Agent: "clutch", Rest: "c123", Create: true, Policy: PolicyFallback}},
		{"clutch/research", Target{Agent: "clutch", Rest: "research", Create: true, Policy: PolicyFallback}},
		{"clutch/research?create=false", Target{Agent: "clutch", Rest: "research", Create: false, Policy: PolicyFallback}},
		{"clutch?policy=strict", Target{Agent: "clutch", Create: true, Policy: PolicyStrict}},
		{"clutch/c1/b1700?policy=broadcast&create=1", Target{Agent: "clutch", Rest: "c1/b1700", Create: true, Policy: PolicyBroadcast}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "/c123", "clutch?policy=bogus", "clutch?create=true&policy=%zz"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) succeeded, want error", bad)
		}
	}
}

// TestTargetString proves Target.String round-trips through ParseTarget,
// including non-default create/policy params.
func TestTargetString(t *testing.T) {
	for _, in := range []string{
		"clutch",
		"clutch/research",
		"clutch/c1/b1700",
		"clutch/research?create=false",
		"clutch?policy=strict",
	} {
		parsed, err := ParseTarget(in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", in, err)
		}
		back, err := ParseTarget(parsed.String())
		if err != nil {
			t.Fatalf("re-parse %q: %v", parsed.String(), err)
		}
		if back != parsed {
			t.Errorf("round-trip %q → %q: %+v != %+v", in, parsed.String(), back, parsed)
		}
	}
}

func newTestIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func active(t *testing.T, idx *session.SessionIndex, key string) {
	t.Helper()
	idx.Upsert(session.SessionIndexEntry{SessionKey: key, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
}

// TestResolve_Ladder proves the full resolution ladder and its precedence:
// exact key → existing named session → chat alias → created named session,
// with an empty Rest resolving to the agent default.
func TestResolve_Ladder(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}

	// A chat aliased "holiday" resolves via the alias rung to its derived key.
	if err := idx.SetChatAliasUnique("clutch", "app", 7, "holiday"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "holiday", Create: true}); err != nil || got.SessionKey != "clutch/c7" || got.Rung != RungAlias {
		t.Fatalf("alias: got %+v, %v; want clutch/c7 via alias", got, err)
	}

	// An existing named session of the same name wins over the alias.
	active(t, idx, "clutch/iholiday")
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "holiday", Create: true}); err != nil || got.SessionKey != "clutch/iholiday" || got.Rung != RungNamed {
		t.Fatalf("named-wins: got %+v, %v", got, err)
	}

	// An exact existing session key wins over everything.
	active(t, idx, "clutch/c99")
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "c99", Create: true}); err != nil || got.SessionKey != "clutch/c99" || got.Rung != RungExact {
		t.Fatalf("exact: got %+v, %v", got, err)
	}

	// A fresh valid name with no alias and no session → created rung.
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "brandnew", Create: true}); err != nil || got.SessionKey != "clutch/ibrandnew" || got.Rung != RungCreated {
		t.Fatalf("created: got %+v, %v", got, err)
	}

	// Same, with Create disabled → ErrUnknownTarget.
	if _, err := r.Resolve(Target{Agent: "clutch", Rest: "brandnew", Create: false}); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("create-disabled err = %v, want ErrUnknownTarget", err)
	}

	// An invalid session name with no matching alias → ErrUnknownTarget.
	if _, err := r.Resolve(Target{Agent: "clutch", Rest: "bad name!/x", Create: true}); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("invalid-name err = %v, want ErrUnknownTarget", err)
	}
}

// TestResolve_Default proves an empty Rest resolves to the agent's default
// session (most recently active root when no default chat is flagged), and
// that an agent with no sessions yields ErrNoSession.
func TestResolve_Default(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}

	if _, err := r.Resolve(Target{Agent: "clutch"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("empty index err = %v, want ErrNoSession", err)
	}

	active(t, idx, "clutch/c42")
	got, err := r.Resolve(Target{Agent: "clutch"})
	if err != nil || got.SessionKey != "clutch/c42" || got.Rung != RungDefault {
		t.Fatalf("default: got %+v, %v", got, err)
	}
}

// TestResolve_AmbiguousAlias proves an alias matching multiple chats surfaces
// session.ErrAliasAmbiguous rather than silently picking one.
func TestResolve_AmbiguousAlias(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}
	for _, chat := range []int64{1, 2} {
		if err := idx.SetChatMetadata("clutch", "app", chat, "alias", "dupe"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Resolve(Target{Agent: "clutch", Rest: "dupe", Create: false}); !errors.Is(err, session.ErrAliasAmbiguous) {
		t.Fatalf("err = %v, want ErrAliasAmbiguous", err)
	}
}

// TestResolve_NilIndex proves the resolver degrades gracefully without an
// index: named targets derive (created rung), defaults error.
func TestResolve_NilIndex(t *testing.T) {
	r := &Resolver{}
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "research", Create: true}); err != nil || got.SessionKey != "clutch/iresearch" || got.Rung != RungCreated {
		t.Fatalf("nil-index named: got %+v, %v", got, err)
	}
	if _, err := r.Resolve(Target{Agent: "clutch"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("nil-index default err = %v, want ErrNoSession", err)
	}
}
