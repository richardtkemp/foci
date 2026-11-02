package memory

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSplitQuotedPhrases(t *testing.T) {
	cases := []struct {
		in      string
		phrases []string
		rest    string
	}{
		{"plain words", nil, "plain words"},
		{`"statement is closed"`, []string{"statement is closed"}, ""},
		{`sqlite "statement is closed" error`, []string{"statement is closed"}, "sqlite   error"},
		{`"a b" "c d"`, []string{"a b", "c d"}, ""},
		{`say "unbalanced`, nil, `say "unbalanced`},
		{`"" empty`, nil, "empty"},
	}
	for _, c := range cases {
		ph, rest := splitQuotedPhrases(c.in)
		if !slices.Equal(ph, c.phrases) || rest != c.rest {
			t.Errorf("splitQuotedPhrases(%q) = %q, %q; want %q, %q", c.in, ph, rest, c.phrases, c.rest)
		}
	}
}

// TestBleveTodoSearchQuotedPhrase is #2816's repro: `"statement is closed"`
// ranked budget tickets about bank statements first, because the quotes were
// ignored and the words matched loosely. The todo holding the exact phrase
// must come first, ahead of a todo that matches the words more often but not
// as a phrase; loose matches still follow it.
//
// The index drops stop words, so a stop word in a phrase matches any one
// word: "statement is closed" also matches "statement was closed". Todo 1
// must not hold "statement <word> closed", or it is an exact hit too.
func TestBleveTodoSearchQuotedPhrase(t *testing.T) {
	idx, _ := testBleveIndex(t)
	now := float64(time.Now().Unix())
	idx.IndexTodo("a", 1, "budget: bank statement closed, statement closed, account closed statement", now)
	idx.IndexTodo("a", 2, "sqlite error: statement is closed during reindex", now)
	idx.IndexTodo("a", 3, "nothing relevant", now)

	ids := func(q, sortOrder string) []int64 {
		t.Helper()
		hits, err := idx.SearchTodos("a", q, sortOrder, 0)
		if err != nil {
			t.Fatalf("SearchTodos(%q): %v", q, err)
		}
		var out []int64
		for _, h := range hits {
			out = append(out, h.TodoID)
		}
		return out
	}
	// Control: unquoted, the loose BM25 order puts the bank-statement todo first.
	if got := ids("statement is closed", ""); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("unquoted control = %v, want [1 2] (the test no longer shows the loose order)", got)
	}
	if got := ids(`"statement is closed"`, ""); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("quoted = %v, want [2 1]: exact phrase first, loose match after", got)
	}
	if got := ids(`sqlite "statement is closed"`, ""); len(got) == 0 || got[0] != 2 {
		t.Errorf("phrase with extra term = %v, want 2 first", got)
	}
	if got := ids(`"closed statement"`, ""); slices.Contains(got, 3) || (len(got) > 0 && got[0] == 2) {
		t.Errorf(`"closed statement" = %v: todo 2 has the words in the other order, it must not rank as exact`, got)
	}
}

// TestTodoSearchQuotedPhraseKeepsTierOnPostSort checks the store keeps the
// exact-phrase hits first when it re-sorts by priority itself.
func TestTodoSearchQuotedPhraseKeepsTierOnPostSort(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	idx, _ := testBleveIndex(t)
	store.SetSearchIndex(idx)

	if _, err := store.Add("a", "bank statement closed early", "high", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("a", "error: statement is closed", "low", ""); err != nil {
		t.Fatal(err)
	}
	items, err := store.Search("a", `"statement is closed"`, &TodoSearchOpts{Sort: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, it := range items {
		got = append(got, it.ID)
	}
	if !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("priority-sorted phrase search = %v, want [2 1] (exact tier first)", got)
	}
}
