package linkwalk

import (
	"maps"
	"slices"
	"testing"
)

// The link functions below count their calls. A correct walk asks for each
// key's links at most once, so more calls than the fixture has keys means the
// walk went round a cycle. At that point the fixture reports the failure and
// then ends the chain, so a broken walk FAILS here instead of hanging the
// package (and, for Down, instead of growing its queue until the host runs out
// of memory). The budget is the fixture's own key count, not a chosen number.

func countedParents(t *testing.T, m map[string]string, keys int) func(string) (string, bool) {
	t.Helper()
	calls := 0
	return func(k string) (string, bool) {
		calls++
		if calls > keys {
			t.Errorf("parent called %d times for %d keys: Up revisits a key, so it loops on a cycle", calls, keys)
			return "", false
		}
		p, ok := m[k]
		return p, ok
	}
}

func countedChildren(t *testing.T, m map[string][]string, keys int) func(string) []string {
	t.Helper()
	calls := 0
	return func(k string) []string {
		calls++
		if calls > keys {
			t.Errorf("children called %d times for %d keys: Down expands a key twice, so it loops on a cycle", calls, keys)
			return nil
		}
		return m[k]
	}
}

func sortedKeys(m map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(m))
}

func TestUp_Chain(t *testing.T) {
	top, cycle := Up("c", countedParents(t, map[string]string{"c": "b", "b": "a"}, 3))
	if top != "a" || cycle {
		t.Errorf("Up = (%q, %v), want (a, false)", top, cycle)
	}
}

func TestUp_StartIsTop(t *testing.T) {
	top, cycle := Up("a", countedParents(t, nil, 1))
	if top != "a" || cycle {
		t.Errorf("Up = (%q, %v), want (a, false)", top, cycle)
	}
}

func TestUp_SelfParent(t *testing.T) {
	top, cycle := Up("a", countedParents(t, map[string]string{"a": "a"}, 1))
	if top != "a" || !cycle {
		t.Errorf("Up = (%q, %v), want (a, true)", top, cycle)
	}
}

// The cycle does not include the start: x -> a -> b -> a. The walk must still
// stop, and report a key on the cycle.
func TestUp_CycleAboveStart(t *testing.T) {
	top, cycle := Up("x", countedParents(t, map[string]string{"x": "a", "a": "b", "b": "a"}, 3))
	if !cycle || (top != "a" && top != "b") {
		t.Errorf("Up = (%q, %v), want a key on the a<->b cycle and cycle=true", top, cycle)
	}
}

func TestDown_Tree(t *testing.T) {
	got := Down("r", countedChildren(t, map[string][]string{"r": {"a", "b"}, "a": {"c"}}, 4))
	if want := []string{"a", "b", "c", "r"}; !slices.Equal(sortedKeys(got), want) {
		t.Errorf("Down = %v, want %v", sortedKeys(got), want)
	}
}

func TestDown_SelfChild(t *testing.T) {
	got := Down("r", countedChildren(t, map[string][]string{"r": {"r", "a"}}, 2))
	if want := []string{"a", "r"}; !slices.Equal(sortedKeys(got), want) {
		t.Errorf("Down = %v, want %v", sortedKeys(got), want)
	}
}

func TestDown_Cycle(t *testing.T) {
	got := Down("r", countedChildren(t, map[string][]string{"r": {"a"}, "a": {"b"}, "b": {"r", "c"}}, 4))
	if want := []string{"a", "b", "c", "r"}; !slices.Equal(sortedKeys(got), want) {
		t.Errorf("Down = %v, want %v", sortedKeys(got), want)
	}
}

// Two paths to one key (a diamond) is not a cycle, but it is the case that
// makes a walk count a key twice if it tracks nothing.
func TestDown_Diamond(t *testing.T) {
	got := Down("r", countedChildren(t, map[string][]string{"r": {"a", "b"}, "a": {"c"}, "b": {"c"}}, 4))
	if want := []string{"a", "b", "c", "r"}; !slices.Equal(sortedKeys(got), want) {
		t.Errorf("Down = %v, want %v", sortedKeys(got), want)
	}
}
