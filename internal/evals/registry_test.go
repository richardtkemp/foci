package evals

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

const humanNumericRubric = "---\n" +
	"kind: human\n" +
	"type: numeric\n" +
	"min: 1\n" +
	"max: 5\n" +
	"---\n" +
	"Rate overall quality.\n"

const humanCategoricalScopedRubric = "---\n" +
	"kind: human\n" +
	"type: categorical\n" +
	"categories:\n" +
	"  - label: good\n" +
	"    value: 1\n" +
	"  - label: bad\n" +
	"    value: 0\n" +
	"select:\n" +
	"  agents:\n" +
	"    - agentA\n" +
	"---\n"

const judgeRubric = "---\n" +
	"kind: judge\n" +
	"type: boolean\n" +
	"judge:\n" +
	"  model: gpt-4o\n" +
	"---\n" +
	"Is the reply honest?\n"

const brokenRubric = "---\n" +
	"type: boolean\n" + // missing kind
	"---\n"

func writeRubric(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestLoad_MissingDir proves a missing rubrics directory is an empty
// registry, not an error — evals are opt-in.
func TestLoad_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(missing dir): unexpected error: %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
}

// TestLoad_GoodAndBadFiles proves per-file errors are kept and logged
// without failing the whole directory load.
func TestLoad_GoodAndBadFiles(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "quality", humanNumericRubric)
	writeRubric(t, dir, "tone", humanCategoricalScopedRubric)
	writeRubric(t, dir, "broken", brokenRubric)

	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	rubrics := r.List()
	if len(rubrics) != 2 {
		t.Fatalf("List() = %d rubrics, want 2: %+v", len(rubrics), rubrics)
	}
	errs := r.Errors()
	brokenPath := filepath.Join(dir, "broken.md")
	if _, ok := errs[brokenPath]; !ok {
		t.Errorf("Errors() = %v, want an entry for %s", errs, brokenPath)
	}
}

// TestList_SortedByName proves listings are stable and ordered.
func TestList_SortedByName(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "zebra", humanNumericRubric)
	writeRubric(t, dir, "alpha", humanNumericRubric)
	writeRubric(t, dir, "mid", humanNumericRubric)

	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var names []string
	for _, rb := range r.List() {
		names = append(names, rb.Name)
	}
	want := []string{"alpha", "mid", "zebra"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names = %v, want %v", names, want)
			break
		}
	}
}

// TestGet proves direct lookup by name.
func TestGet(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "quality", humanNumericRubric)
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := r.Get("quality"); !ok {
		t.Error("Get(quality) not found")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) unexpectedly found")
	}
}

// TestHuman proves Human filters to kind==human AND the rubric's Select,
// distinguishing both from a non-human (judge) rubric that must never
// appear.
func TestHuman(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "quality", humanNumericRubric)        // human, unrestricted
	writeRubric(t, dir, "tone", humanCategoricalScopedRubric) // human, agents: [agentA]
	writeRubric(t, dir, "honesty", judgeRubric)               // judge — must never show up
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	agentA := r.Human("agentA", "chat")
	var namesA []string
	for _, rb := range agentA {
		namesA = append(namesA, rb.Name)
	}
	sort.Strings(namesA)
	if len(namesA) != 2 || namesA[0] != "quality" || namesA[1] != "tone" {
		t.Errorf("Human(agentA) = %v, want [quality tone]", namesA)
	}

	agentB := r.Human("agentB", "chat")
	var namesB []string
	for _, rb := range agentB {
		namesB = append(namesB, rb.Name)
	}
	if len(namesB) != 1 || namesB[0] != "quality" {
		t.Errorf("Human(agentB) = %v, want [quality] (tone restricted to agentA, honesty is a judge rubric)", namesB)
	}
}

// TestSetConfigID_ReloadUnchanged proves ConfigID survives a reload of an
// unchanged file, but is dropped and reported as changed once the file's
// version bumps.
func TestSetConfigID_ReloadUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "axis", humanNumericRubric)
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SetConfigID("axis", "cfg-123")
	rb, _ := r.Get("axis")
	if rb.ConfigID != "cfg-123" {
		t.Fatalf("ConfigID after SetConfigID = %q, want cfg-123", rb.ConfigID)
	}

	// Reload without touching the file: ConfigID must survive, nothing changed.
	var changed []*Rubric
	r.OnChange = func(c []*Rubric) { changed = c }
	err = r.reload()
	if err != nil {
		t.Fatalf("Reload (unchanged): %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("Reload (unchanged) changed = %+v, want none", changed)
	}
	rb, _ = r.Get("axis")
	if rb.ConfigID != "cfg-123" {
		t.Errorf("ConfigID after unchanged reload = %q, want cfg-123 (carried over)", rb.ConfigID)
	}

	// Bump the version: ConfigID must be dropped, and axis reported changed.
	versioned := "---\n" +
		"kind: human\n" +
		"type: numeric\n" +
		"min: 1\n" +
		"max: 5\n" +
		"version: 2\n" +
		"---\n" +
		"Rate overall quality.\n"
	writeRubric(t, dir, "axis", versioned)
	changed = nil
	err = r.reload()
	if err != nil {
		t.Fatalf("Reload (version bump): %v", err)
	}
	if len(changed) != 1 || changed[0].Name != "axis" {
		t.Fatalf("Reload (version bump) changed = %+v, want [axis]", changed)
	}
	rb, _ = r.Get("axis")
	if rb.ConfigID != "" {
		t.Errorf("ConfigID after version bump = %q, want dropped (empty)", rb.ConfigID)
	}
}

// TestOnChange_OnlyAddedOrChanged proves OnChange fires with exactly the
// rubrics that are new or different since the previous load — an unchanged
// sibling must not appear.
func TestOnChange_OnlyAddedOrChanged(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "steady", humanNumericRubric)
	writeRubric(t, dir, "mutating", humanNumericRubric)
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got []string
	r.OnChange = func(changed []*Rubric) {
		for _, rb := range changed {
			got = append(got, rb.Name)
		}
	}

	mutated := "---\n" +
		"kind: human\n" +
		"type: numeric\n" +
		"min: 1\n" +
		"max: 5\n" +
		"version: 2\n" +
		"---\n" +
		"Rate overall quality.\n"
	writeRubric(t, dir, "mutating", mutated)
	writeRubric(t, dir, "added", humanNumericRubric)

	if err := r.reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "added" || got[1] != "mutating" {
		t.Errorf("OnChange saw %v, want [added mutating] (steady must be excluded)", got)
	}
}

// TestReload_RemovesDeletedFile proves a rubric whose file disappears is
// removed from the registry on the next reload.
func TestReload_RemovesDeletedFile(t *testing.T) {
	dir := t.TempDir()
	writeRubric(t, dir, "keep", humanNumericRubric)
	writeRubric(t, dir, "gone", humanNumericRubric)
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := r.Get("gone"); !ok {
		t.Fatal("Get(gone) not found before removal")
	}
	if err := os.Remove(filepath.Join(dir, "gone.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := r.reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := r.Get("gone"); ok {
		t.Error("Get(gone) still found after its file was removed")
	}
	if _, ok := r.Get("keep"); !ok {
		t.Error("Get(keep) should still be found")
	}
}

// TestWatch_PicksUpNewFile drives the real fsnotify path: Watch on an
// initially-empty directory, then a new rubric file dropped in must become
// visible via Get within a few seconds.
func TestWatch_PicksUpNewFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rubrics")
	r, err := Load(dir) // directory does not exist yet — tolerated
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.Watch(); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(r.Close)

	writeRubric(t, dir, "newax", humanNumericRubric)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := r.Get("newax"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("newax never appeared in the registry after 3s of watching")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
