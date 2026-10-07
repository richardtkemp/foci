package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
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
// always reflects the current file. The returned Store is read-only: it
// cannot Save or mutate (rootOnly).
func (s *Store) ForAgent(agentID string) *Store {
	return &Store{src: s.src, agentID: agentID}
}

// rootOnly rejects write operations on per-agent views. Views are read-only
// windows over the shared source — letting one mutate would change every
// other agent's secrets — so, like Save, they refuse.
func (s *Store) rootOnly() error {
	if s.agentID != "" {
		return fmt.Errorf("secrets store for agent %q is read-only — use the root store", s.agentID)
	}
	return nil
}

// HasAgentRestrictions reports whether any section of the current file has
// allowed_agents or denied_agents. That is a file-level question, so views
// answer it like the root (the caller is startup wiring on the root store).
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
// The mutator first re-reads the file, so an external edit not yet observed
// is merged rather than overwritten. The change is visible to every store
// sharing the source; call Save on the root store to persist it.
func (s *Store) Set(name, value string) error {
	if err := s.rootOnly(); err != nil {
		return err
	}
	s.src.mutate(func(st *fileState) *fileState {
		values := cloneMap(st.values)
		values[name] = value
		return st.withValues(values)
	})
	return nil
}

// Remove deletes a secret by its flat key. Returns true if found.
func (s *Store) Remove(name string) (bool, error) {
	if err := s.rootOnly(); err != nil {
		return false, err
	}
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
	return removed, nil
}

// Save writes the current secrets back to the TOML file. Only the root store
// can save; per-agent views return an error. The write is deliberately not
// atomic (no write-and-rename): the file's root:foci-secrets ownership must
// survive, and a rename would reset it (see docs/SECRETS.md). See
// source.save for the locking that keeps concurrent readers safe.
func (s *Store) Save() error {
	if err := s.rootOnly(); err != nil {
		return err
	}
	return s.src.save()
}

// cloneMap copies m (copy-on-write support for the mutators).
func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
