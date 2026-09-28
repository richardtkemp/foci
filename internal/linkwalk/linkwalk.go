// Package linkwalk follows parent and child links in data that foci does not
// fully control: session_index rows, CC's subagent sidecars, stream events.
//
// That data can hold a cycle — a row that names itself as its parent, or two
// rows that name each other. A walk that trusts the links then never ends: an
// upward walk spins, a downward walk grows its queue without limit (#1581).
//
// Every walk here keeps a visited set and stops at the first repeated key. That
// set is the whole bound: a walk visits each distinct key at most once, so it
// ends after at most as many steps as there are keys, and a legitimately deep
// chain is never cut short. A fixed depth cap would need a number that is both
// larger than any real chain and small enough to matter, and no such number is
// known.
package linkwalk

// Up follows parent links from start. parent returns the next key and true, or
// false when the key has no parent (the top of the chain).
//
// It returns the last key it reached. cycle is true when the walk stopped
// because a key repeated; top is then the repeated key, which lies on the cycle.
func Up[K comparable](start K, parent func(K) (K, bool)) (top K, cycle bool) {
	seen := make(map[K]struct{})
	cur := start
	for {
		if _, dup := seen[cur]; dup {
			return cur, true
		}
		seen[cur] = struct{}{}
		next, ok := parent(cur)
		if !ok {
			return cur, false
		}
		cur = next
	}
}

// Down returns root and every key reachable from it through children, each
// once. A child link back to a key already reached (a self-link, or a cycle) is
// ignored.
func Down[K comparable](root K, children func(K) []K) map[K]struct{} {
	reached := map[K]struct{}{}
	queue := []K{root}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if _, dup := reached[k]; dup {
			continue
		}
		reached[k] = struct{}{}
		queue = append(queue, children(k)...)
	}
	return reached
}
