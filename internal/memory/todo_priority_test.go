package memory

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/sqlite"
)

// TestNormalizePriority pins the alias table: case, whitespace, and the
// unambiguous short forms coerce; anything else is rejected with an error
// naming the valid values (#2071).
func TestNormalizePriority(t *testing.T) {
	ok := map[string]string{
		"":        "",
		"high":    "high",
		"HIGH":    "high",
		" High ":  "high",
		"hi":      "high",
		"h":       "high",
		"medium":  "medium",
		"Medium":  "medium",
		"med":     "medium",
		"MED":     "medium",
		"mid":     "medium",
		"m":       "medium",
		"low":     "low",
		"lo":      "low",
		"L":       "low",
		"\tlow\n": "low",
	}
	for in, want := range ok {
		got, err := NormalizePriority(in)
		if err != nil || got != want {
			t.Errorf("NormalizePriority(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	for _, in := range []string{"urgent", "p1", "hgih", "medium-high", "3", "!high"} {
		got, err := NormalizePriority(in)
		if err == nil {
			t.Errorf("NormalizePriority(%q) = %q, nil; want an error", in, got)
			continue
		}
		for _, v := range todoPriorities {
			if !strings.Contains(err.Error(), v) {
				t.Errorf("NormalizePriority(%q) error %q does not list valid value %q", in, err, v)
			}
		}
	}
}

// TestTodoAddEditCoercePriority is the #2071 fail-arm: on the old store, Add
// and Edit saved "med"/"HI" verbatim, and --sort priority then ranked those
// rows ahead of every real 'high'.
func TestTodoAddEditCoercePriority(t *testing.T) {
	store := newTestTodoStore(t)

	idMed, err := store.Add("a", "med item", "med", "")
	if err != nil {
		t.Fatalf("Add med: %v", err)
	}
	idHigh, err := store.Add("a", "high item", "high", "")
	if err != nil {
		t.Fatalf("Add high: %v", err)
	}
	idLow, err := store.Add("a", "low item", "low", "")
	if err != nil {
		t.Fatalf("Add low: %v", err)
	}
	if _, err := store.Edit("a", idLow, "", " HI ", "", false, false); err != nil {
		t.Fatalf("Edit HI: %v", err)
	}

	for id, want := range map[int64]string{idMed: "medium", idHigh: "high", idLow: "high"} {
		item, err := store.Get("a", id)
		if err != nil {
			t.Fatalf("Get #%d: %v", id, err)
		}
		if item.Priority != want {
			t.Errorf("#%d stored priority %q, want %q", id, item.Priority, want)
		}
	}

	items, err := store.List("a", "open", nil, "", "priority", false, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 || items[2].ID != idMed {
		var got []int64
		for _, it := range items {
			got = append(got, it.ID)
		}
		t.Errorf("--sort priority order = %v, want the medium item #%d last", got, idMed)
	}

	// A filter spelled with an alias still finds the canonical rows.
	items, err = store.List("a", "", nil, "med", "", false, 0)
	if err != nil || len(items) != 1 || items[0].ID != idMed {
		t.Errorf("List p:med = %v, %v; want just #%d", items, err, idMed)
	}
}

// TestTodoAddEditRejectUnknownPriority: a value that can't be mapped is
// refused, and nothing is written.
func TestTodoAddEditRejectUnknownPriority(t *testing.T) {
	store := newTestTodoStore(t)

	if _, err := store.Add("a", "x", "urgent", ""); err == nil || !strings.Contains(err.Error(), "high, medium, low") {
		t.Errorf("Add urgent: err = %v, want an error listing high, medium, low", err)
	}
	if n, _ := store.CountList("a", "", nil, ""); n != 0 {
		t.Errorf("rejected Add still inserted %d row(s)", n)
	}

	id, _ := store.Add("a", "orig", "low", "")
	if _, err := store.Edit("a", id, "changed", "urgent", "", false, false); err == nil {
		t.Error("Edit urgent: want an error")
	}
	item, _ := store.Get("a", id)
	if item.Priority != "low" || item.Text != "orig" {
		t.Errorf("rejected Edit wrote something: priority %q text %q", item.Priority, item.Text)
	}
}

// TestTodoOpenMigratesStoredPriorityAliases: rows already stored with an alias
// (the closed 'med' todos in agents' dbs) are rewritten when the db is opened,
// without bumping updated_at; unmappable values are left alone.
func TestTodoOpenMigratesStoredPriorityAliases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "todo.db")
	store, err := NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	for _, text := range []string{"a", "b", "c"} {
		if _, err := store.Add("ag", text, "", ""); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := store.Transition("ag", 1, "done", "closed"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	_ = store.Close()

	// Plant legacy values behind the store's back, as older builds wrote them.
	raw, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for id, p := range map[int]string{1: "med", 2: "Hi", 3: "urgent"} {
		if _, err := raw.Exec(`UPDATE todos SET priority = ? WHERE id = ?`, p, id); err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	var before sql.NullString
	_ = raw.QueryRow(`SELECT updated_at FROM todos WHERE id = 1`).Scan(&before)
	_ = raw.Close()

	store, err = NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store.Close() }()

	want := map[int64]string{1: "medium", 2: "high", 3: "urgent"}
	for id, p := range want {
		item, err := store.Get("ag", id)
		if err != nil {
			t.Fatalf("Get #%d: %v", id, err)
		}
		if item.Priority != p {
			t.Errorf("#%d priority after reopen = %q, want %q", id, item.Priority, p)
		}
	}
	var after sql.NullString
	_ = store.db.QueryRow(`SELECT updated_at FROM todos WHERE id = 1`).Scan(&after)
	if before != after {
		t.Errorf("migration bumped updated_at: %v -> %v", before, after)
	}
}
