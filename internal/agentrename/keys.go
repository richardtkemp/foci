package agentrename

import (
	"bytes"
	"encoding/json"
	"strings"

	"foci/internal/session"
)

// ids is one rename: every rewrite in this package maps old to new.
type ids struct{ old, new string }

// rewriteKey rewrites s when it is a session key of agent old — optionally
// followed by an '@…' tail (turn ids, call keys, scope keys) — changing only
// its first segment. Any other string, including one that merely contains
// old ("bobby" for "bob", "x clutch/c1", another agent's key), is unchanged.
func (r ids) rewriteKey(s string) (string, bool) {
	// A key never holds whitespace; this keeps prose that starts with a key
	// ("bob/c1 in prose") out — ParseSessionKey alone accepts any id text.
	if !strings.HasPrefix(s, r.old+"/") || strings.ContainsAny(s, " \t\r\n") {
		return s, false
	}
	key, _, _ := strings.Cut(s, "@")
	if _, err := session.ParseSessionKey(key); err != nil {
		return s, false
	}
	return r.new + s[len(r.old):], true
}

// RewriteSessionKey is the key rule the disk rename applies, for in-memory
// state that holds session keys (the app hub's conversation bindings, #2787):
// key with agent old renamed to new when it is a session key of old, else
// key unchanged.
func RewriteSessionKey(old, new, key string) string {
	out, _ := ids{old, new}.rewriteKey(key)
	return out
}

// rewriteValue rewrites a stored value that holds session keys: a value that
// is itself a key (rewriteKey), or a JSON object/array, in which every string
// — and every object member name — that is a key of agent old is rewritten.
// The JSON stays valid with the same shape; other text is untouched. Any
// other value is returned unchanged.
func (r ids) rewriteValue(s string) (string, bool) {
	if out, ok := r.rewriteKey(s); ok {
		return out, true
	}
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') || !strings.Contains(s, r.old+"/") {
		return s, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return s, false
	}
	nv, changed := r.rewriteJSON(v)
	if !changed {
		return s, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(nv); err != nil {
		return s, false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

func (r ids) rewriteJSON(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		return r.rewriteKey(t)
	case []any:
		changed := false
		for i, e := range t {
			if ne, ok := r.rewriteJSON(e); ok {
				t[i] = ne
				changed = true
			}
		}
		return t, changed
	case map[string]any:
		changed := false
		out := make(map[string]any, len(t))
		for k, e := range t {
			ne, ok := r.rewriteJSON(e)
			changed = changed || ok
			if nk, kok := r.rewriteKey(k); kok {
				k = nk
				changed = true
			}
			out[k] = ne
		}
		return out, changed
	}
	return v, false
}

// rewriteMetaKey rewrites an agent_metadata key of the form "<kind>:<session
// key>" (depth:<key>), whose tail is a session key of agent old.
func (r ids) rewriteMetaKey(k string) (string, bool) {
	kind, tail, ok := strings.Cut(k, ":")
	if !ok {
		return k, false
	}
	nt, ok := r.rewriteKey(tail)
	if !ok {
		return k, false
	}
	return kind + ":" + nt, true
}
