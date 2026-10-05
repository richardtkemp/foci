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

// TestResolve_CreateDefault proves that when the default rung finds no
// non-archived session, a resolver with CreateDefault set mints one (delivery
// paths), while one without it errors ErrNoSession (warm paths).
func TestResolve_CreateDefault(t *testing.T) {
	idx := newTestIndex(t)

	// Without the hook: empty index → ErrNoSession.
	if _, err := (&Resolver{Index: idx}).Resolve(Target{Agent: "clutch"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no hook: err = %v, want ErrNoSession", err)
	}

	// With the hook: the created session key comes back on the RungCreated rung.
	called := ""
	r := &Resolver{Index: idx, CreateDefault: func(agentID string) (string, error) {
		called = agentID
		return agentID + "/cNEW", nil
	}}
	got, err := r.Resolve(Target{Agent: "clutch"})
	if err != nil || got.SessionKey != "clutch/cNEW" || got.Rung != RungCreated {
		t.Fatalf("hook: got %+v, %v", got, err)
	}
	if called != "clutch" {
		t.Errorf("hook called with %q, want clutch", called)
	}

	// Hook error propagates (no client to host the conversation).
	rErr := &Resolver{Index: idx, CreateDefault: func(string) (string, error) {
		return "", errors.New("no app connection")
	}}
	if _, err := rErr.Resolve(Target{Agent: "clutch"}); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("hook error: err = %v, want the hook's error", err)
	}

	// The hook does NOT fire when a non-archived default exists.
	active(t, idx, "clutch/c42")
	if got, err := r.Resolve(Target{Agent: "clutch"}); err != nil || got.SessionKey != "clutch/c42" || got.Rung != RungDefault {
		t.Fatalf("existing default: got %+v, %v", got, err)
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

// TestResolve_AliasWithKeyCharacters is the #2157 safety argument for allowing
// '/' and ':' in chat aliases. Such an alias must resolve through every target
// form, and must never shadow a real session key: the ladder tries the exact key
// and the named session BEFORE the alias rung, so a key always beats an alias
// that happens to spell it.
func TestResolve_AliasWithKeyCharacters(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}

	for chat, alias := range map[int64]string{7: "OCN: committee software", 8: "infra/dns", 9: "a/b:c/d"} {
		if err := idx.SetChatAliasUnique("clutch", "app", chat, alias); err != nil {
			t.Fatalf("SetChatAliasUnique(%q): %v", alias, err)
		}
		want := session.NewChatSessionKey("clutch", chat)
		// The canonical string form, as send_to_session and the CLI parse it.
		tgt, err := ParseTarget("clutch/" + alias)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", alias, err)
		}
		if got, err := r.Resolve(tgt); err != nil || got.SessionKey != want || got.Rung != RungAlias {
			t.Errorf("alias %q: got %+v, %v; want %s via alias", alias, got, err, want)
		}
	}

	// An alias spelling a real key does not capture it: the key wins.
	active(t, idx, "clutch/c99/b123")
	if err := idx.SetChatAliasUnique("clutch", "app", 10, "c99/b123"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(Target{Agent: "clutch", Rest: "c99/b123"}); err != nil || got.SessionKey != "clutch/c99/b123" || got.Rung != RungExact {
		t.Fatalf("key-shaped alias: got %+v, %v; want the real key via exact", got, err)
	}
}

// TestResolverParseTarget_LiteralAliasFirst is the #2158 ruling: a target whose
// whole Rest literally names a chat alias resolves to that alias even when it
// contains '?'; only when nothing matches is '?' read as the start of options.
func TestResolverParseTarget_LiteralAliasFirst(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}

	for chat, alias := range map[int64]string{7: "what next?", 8: "research?create=false", 9: "Q?"} {
		if err := idx.SetChatAliasUnique("clutch", "app", chat, alias); err != nil {
			t.Fatalf("SetChatAliasUnique(%q): %v", alias, err)
		}
		tgt, err := r.ParseTarget("clutch/" + alias)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", alias, err)
		}
		want := Target{Agent: "clutch", Rest: alias, Create: true, Policy: PolicyFallback}
		if tgt != want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", alias, tgt, want)
		}
		if got, err := r.Resolve(tgt); err != nil || got.SessionKey != session.NewChatSessionKey("clutch", chat) || got.Rung != RungAlias {
			t.Errorf("alias %q: got %+v, %v; want chat %d via alias", alias, got, err, chat)
		}
	}

	// Case-insensitive, like the alias rung itself.
	if tgt, err := r.ParseTarget("clutch/WHAT NEXT?"); err != nil || tgt.Rest != "WHAT NEXT?" {
		t.Errorf("case-insensitive literal: got %+v, %v", tgt, err)
	}

	// No literal match: '?' starts options, exactly as the package ParseTarget.
	for _, in := range []string{"clutch/notes?create=false", "clutch/q?policy=strict", "clutch?policy=strict", "clutch/c1/b1700?policy=broadcast"} {
		got, err := r.ParseTarget(in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", in, err)
		}
		want, _ := ParseTarget(in)
		if got != want {
			t.Errorf("ParseTarget(%q) = %+v, want options parse %+v", in, got, want)
		}
	}
	if _, err := r.ParseTarget("clutch?policy=bogus"); err == nil {
		t.Error("bad options with no literal alias: want error")
	}

	// An ambiguous literal still counts as a match: the ambiguity surfaces from
	// Resolve instead of the '?' being silently re-read as options.
	for _, chat := range []int64{20, 21} {
		if err := idx.SetChatMetadata("clutch", "app", chat, "alias", "dup?policy=strict"); err != nil {
			t.Fatal(err)
		}
	}
	tgt, err := r.ParseTarget("clutch/dup?policy=strict")
	if err != nil || tgt.Rest != "dup?policy=strict" {
		t.Fatalf("ambiguous literal: got %+v, %v", tgt, err)
	}
	if _, err := r.Resolve(tgt); !errors.Is(err, session.ErrAliasAmbiguous) {
		t.Errorf("ambiguous literal resolve err = %v, want ErrAliasAmbiguous", err)
	}

	// Without an index there is nothing to match literally.
	if got, err := (&Resolver{}).ParseTarget("clutch/what next?"); err != nil || got.Rest != "what next" {
		t.Errorf("nil index: got %+v, %v; want options parse", got, err)
	}
}

// TestResolve_KeyShapedAlias proves the #2158 item-1 case at the resolver: an
// ordinary alias that happens to parse as a session key ("important" reads as
// clutch/important, "c5" as clutch/c5) reaches the alias rung when no such
// session exists. A real existing session still wins (the #2157 invariant).
func TestResolve_KeyShapedAlias(t *testing.T) {
	idx := newTestIndex(t)
	r := &Resolver{Index: idx}
	for chat, alias := range map[int64]string{7: "important", 8: "c5", 9: "c5/b123"} {
		if err := idx.SetChatAliasUnique("clutch", "app", chat, alias); err != nil {
			t.Fatal(err)
		}
		if got, err := r.Resolve(Target{Agent: "clutch", Rest: alias}); err != nil || got.SessionKey != session.NewChatSessionKey("clutch", chat) || got.Rung != RungAlias {
			t.Errorf("alias %q: got %+v, %v; want chat %d via alias", alias, got, err, chat)
		}
	}
}
