package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
)

// GeneratePassphrase picks wordCount random words from the EFF Short Wordlist
// using crypto/rand and joins them with hyphens. 5 words ≈ 52 bits of entropy.
// Example: "maple-thunder-basket-olive-crane".
func GeneratePassphrase(wordCount int) (string, error) {
	if wordCount < 1 {
		return "", fmt.Errorf("word count must be at least 1")
	}
	n := big.NewInt(int64(len(effShortWordlist)))
	words := make([]string, wordCount)
	for i := range words {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("crypto/rand: %w", err)
		}
		words[i] = effShortWordlist[idx.Int64()]
	}
	return strings.Join(words, "-"), nil
}

// Default paths that the exec tool should refuse to read.
var defaultBlockedPaths = []string{
	"secrets.toml",
	"/proc/self/environ",
}

// Store holds the secrets of one secrets.toml file. The root store returned
// by Load can mutate and Save the file; ForAgent returns per-agent views
// that share the same underlying source, so every store — including ones
// handed to tools long ago — sees file edits and in-process mutations on its
// next read, with no restart (#1269). Stores are safe for concurrent use.
// The zero value is not usable; construct stores with Load.
type Store struct {
	src     *source
	agentID string // "" is the root store; otherwise this view's agent ID
}

// Load reads secrets from a TOML file. Returns an empty store (not error) if
// the file doesn't exist; if the file is created later it is loaded on the
// next use. Every read re-checks the file (one os.Stat: modification time at
// full resolution, size, inode identity) and re-parses it when it changed —
// see source.current. A file that changed but cannot be re-parsed keeps the
// last good contents and logs one warning per file state.
func Load(path string) (*Store, error) {
	src := &source{
		path: path,
		// The secrets file itself is always blocked, even when missing.
		blocked: append(append([]string{}, defaultBlockedPaths...), path),
	}
	st, err := parseFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			src.state, src.loaded = emptyState(), statFile(path)
			return &Store{src: src}, nil
		}
		return nil, err
	}
	src.state, src.loaded = st, statFile(path)
	return &Store{src: src}, nil
}

// ForAgent returns a Store scoped to the given agent ID, sharing the same
// source as s. Agent-specific values overlay globals; keys not overridden
// fall back to globals; global sections with allowed_agents/denied_agents
// are filtered out. That filtering is re-applied on every use, so the view
// always reflects the current file. The returned Store cannot Save.
func (s *Store) ForAgent(agentID string) *Store {
	return &Store{src: s.src, agentID: agentID}
}

// HasAgentRestrictions reports whether any section has allowed_agents or denied_agents.
func (s *Store) HasAgentRestrictions() bool {
	st := s.src.current()
	return len(st.allowedAgents) > 0 || len(st.deniedAgents) > 0
}

// Get returns a secret value by its flat key (e.g. "anthropic.setup_token").
func (s *Store) Get(name string) (string, bool) {
	return s.src.current().valueFor(s.agentID, name)
}

// Names returns all secret names (keys) visible to this store, sorted.
func (s *Store) Names() []string {
	return s.src.current().namesFor(s.agentID)
}

// Set adds or updates a secret value by its flat key (e.g. "section.key").
// The change is visible to every store sharing the source; call Save on the
// root store to persist it.
func (s *Store) Set(name, value string) {
	s.src.mutate(func(st *fileState) *fileState {
		values := cloneMap(st.values)
		values[name] = value
		return st.withValues(values)
	})
}

// Remove deletes a secret by its flat key. Returns true if found.
func (s *Store) Remove(name string) bool {
	removed := false
	s.src.mutate(func(st *fileState) *fileState {
		if _, ok := st.values[name]; !ok {
			return st
		}
		values := cloneMap(st.values)
		delete(values, name)
		removed = true
		return st.withValues(values)
	})
	return removed
}

// Save writes the current secrets back to the TOML file. Only the root store
// can save; per-agent views return an error. The write is deliberately not
// atomic (no write-and-rename): the file's root:foci-secrets ownership must
// survive, and a rename would reset it (see docs/SECRETS.md).
func (s *Store) Save() error {
	if s.agentID != "" {
		return fmt.Errorf("secrets store for agent %q cannot save — save via the root store", s.agentID)
	}
	st := s.src.snapshot()

	var buf strings.Builder

	// Write global sections
	sections := flatKeysToSections(st.values)
	secNames := sortedKeyUnion(keysOf(sections), keysOf(st.allowedHosts), keysOf(st.allowedAgents), keysOf(st.deniedAgents), keysOf(st.allowedInBody))
	for i, sec := range secNames {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[%s]\n", sec)
		writeKeyValues(&buf, sections[sec])
		writeStringArrayField(&buf, "allowed_hosts", st.allowedHosts[sec])
		writeStringArrayField(&buf, "allowed_agents", st.allowedAgents[sec])
		writeStringArrayField(&buf, "denied_agents", st.deniedAgents[sec])
		writeStringArrayField(&buf, "allowed_in_body", st.allowedInBody[sec])
	}

	// Write [agents.*] sections
	agentIDs := sortedKeyUnion(keysOf(st.agentValues), keysOf(st.agentHosts), keysOf(st.agentAllowedInBody))
	for _, agentID := range agentIDs {
		agentSections := flatKeysToSections(st.agentValues[agentID])
		subSecs := sortedKeyUnion(keysOf(agentSections), keysOf(st.agentHosts[agentID]), keysOf(st.agentAllowedInBody[agentID]))
		for _, sec := range subSecs {
			buf.WriteByte('\n')
			fmt.Fprintf(&buf, "[agents.%s.%s]\n", agentID, sec)
			writeKeyValues(&buf, agentSections[sec])
			writeStringArrayField(&buf, "allowed_hosts", st.agentHosts[agentID][sec])
			writeStringArrayField(&buf, "allowed_in_body", st.agentAllowedInBody[agentID][sec])
		}
	}

	if err := os.WriteFile(s.src.path, []byte(buf.String()), 0600); err != nil {
		return err
	}
	s.src.markSaved()
	return nil
}

// cloneMap copies m (copy-on-write support for the mutators).
func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// flatKeysToSections groups "section.key" flat keys into a nested map.
func flatKeysToSections(flat map[string]string) map[string]map[string]string {
	sections := make(map[string]map[string]string)
	for k, v := range flat {
		parts := strings.SplitN(k, ".", 2)
		if len(parts) != 2 {
			continue
		}
		sec, key := parts[0], parts[1]
		if sections[sec] == nil {
			sections[sec] = make(map[string]string)
		}
		sections[sec][key] = v
	}
	return sections
}

// keysOf returns the keys of any map[string]V as a slice.
func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// sortedKeyUnion returns the sorted union of keys from multiple slices.
func sortedKeyUnion(slices ...[]string) []string {
	seen := make(map[string]bool)
	for _, s := range slices {
		for _, k := range s {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeKeyValues writes sorted key = value pairs in TOML format.
// Integer values are written unquoted; all others are quoted.
func writeKeyValues(buf *strings.Builder, pairs map[string]string) {
	if len(pairs) == 0 {
		return
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := strconv.ParseInt(pairs[k], 10, 64); err == nil {
			fmt.Fprintf(buf, "%s = %s\n", k, pairs[k])
		} else {
			fmt.Fprintf(buf, "%s = %q\n", k, pairs[k])
		}
	}
}

// writeStringArrayField writes a TOML array field (e.g. allowed_hosts, allowed_agents) if non-empty.
func writeStringArrayField(buf *strings.Builder, key string, values []string) {
	if len(values) == 0 {
		return
	}
	buf.WriteString(key)
	buf.WriteString(" = [")
	for i, v := range values {
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(buf, "%q", v)
	}
	buf.WriteString("]\n")
}
