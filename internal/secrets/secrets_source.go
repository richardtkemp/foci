package secrets

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"foci/internal/log"

	"github.com/BurntSushi/toml"
)

// secretsLog is the package's scoped logger; reload warnings go through it.
var secretsLog = log.NewComponentLogger("secrets")

// stampKind classifies one observation of the secrets file.
type stampKind int

const (
	stampFile      stampKind = iota // the file exists
	stampMissing                    // the file does not exist
	stampStatError                  // os.Stat failed with a non-NotExist error
)

// fileStamp is one observation of the secrets file's identity: whether it
// exists and, if so, its inode, modification time and size. Two stamps being
// "the same" (same) is the definition of "the file has not changed" used to
// decide whether the next read must re-parse it (#1269).
type fileStamp struct {
	kind stampKind
	info os.FileInfo // nil unless kind == stampFile
}

// statFile observes the file at path right now.
func statFile(path string) fileStamp {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return fileStamp{kind: stampFile, info: info}
	case os.IsNotExist(err):
		return fileStamp{kind: stampMissing}
	default:
		return fileStamp{kind: stampStatError}
	}
}

// same reports whether a and b are observations of an unchanged file. Two
// present observations must agree on file identity (os.SameFile — an editor
// that replaces the file by rename yields a different inode), modification
// time at full resolution and size. missing/missing and statError/statError
// each count as one state: a persistent failure is retried only when the
// observation changes.
func (a fileStamp) same(b fileStamp) bool {
	if a.kind != b.kind {
		return false
	}
	if a.kind != stampFile {
		return true
	}
	return os.SameFile(a.info, b.info) &&
		a.info.ModTime().Equal(b.info.ModTime()) &&
		a.info.Size() == b.info.Size()
}

// fileState is the immutable result of one successful parse of secrets.toml
// (or one in-process mutation). It is published by pointer swap and never
// modified afterwards; mutators build a copy with exactly the map they touch
// cloned (copy-on-write, the with* helpers). Immutability is what lets
// readers use a state snapshot without copying it under the lock.
type fileState struct {
	values             map[string]string               // "section.key" → value
	allowedHosts       map[string][]string             // section → allowed hosts
	allowedAgents      map[string][]string             // section → agent whitelist
	deniedAgents       map[string][]string             // section → agent blacklist
	allowedInBody      map[string][]string             // section → key names allowed in request body
	agentValues        map[string]map[string]string   // agent ID → "section.key" → value
	agentHosts         map[string]map[string][]string // agent ID → section → allowed hosts
	agentAllowedInBody map[string]map[string][]string // agent ID → section → key names allowed in body
}

// emptyState is the state of a secrets file with nothing in it.
func emptyState() *fileState {
	return &fileState{
		values:             make(map[string]string),
		allowedHosts:       make(map[string][]string),
		allowedAgents:      make(map[string][]string),
		deniedAgents:       make(map[string][]string),
		allowedInBody:      make(map[string][]string),
		agentValues:        make(map[string]map[string]string),
		agentHosts:         make(map[string]map[string][]string),
		agentAllowedInBody: make(map[string]map[string][]string),
	}
}

// withValues returns a copy of st with values replaced; the other maps are
// shared per fileState's immutability contract.
func (st *fileState) withValues(values map[string]string) *fileState {
	next := *st
	next.values = values
	return &next
}

// withAllowedHosts returns a copy of st with allowedHosts replaced.
func (st *fileState) withAllowedHosts(hosts map[string][]string) *fileState {
	next := *st
	next.allowedHosts = hosts
	return &next
}

// withAllowedInBody returns a copy of st with allowedInBody replaced.
func (st *fileState) withAllowedInBody(keys map[string][]string) *fileState {
	next := *st
	next.allowedInBody = keys
	return &next
}

// agentAllowed reports whether agentID may see the given section under the
// allowed_agents/denied_agents rules. No restrictions means allowed.
func (st *fileState) agentAllowed(agentID, section string) bool {
	if allowed, ok := st.allowedAgents[section]; ok {
		for _, a := range allowed {
			if a == agentID {
				return true
			}
		}
		return false
	}
	if denied, ok := st.deniedAgents[section]; ok {
		for _, a := range denied {
			if a == agentID {
				return false
			}
		}
	}
	return true
}

// valueFor returns the value of name ("section.key") as seen by agentID: the
// agent's own override always wins, then the global value if the agent may
// see its section. The root (agentID "") sees every global value.
func (st *fileState) valueFor(agentID, name string) (string, bool) {
	if agentID != "" {
		if v, ok := st.agentValues[agentID][name]; ok {
			return v, true
		}
		section, _, dotted := strings.Cut(name, ".")
		if !dotted || section == "" || !st.agentAllowed(agentID, section) {
			return "", false
		}
	}
	v, ok := st.values[name]
	return v, ok
}

// hostsFor returns the allowed_hosts for a section as seen by agentID: the
// agent table's list replaces the global one for that section when present,
// else the global list applies if the agent may see the section. The root
// sees the global list unfiltered.
func (st *fileState) hostsFor(agentID, section string) []string {
	if agentID != "" {
		if hosts, ok := st.agentHosts[agentID][section]; ok {
			return hosts
		}
		if !st.agentAllowed(agentID, section) {
			return nil
		}
	}
	return st.allowedHosts[section]
}

// bodyKeysFor returns the allowed_in_body list for a section as seen by
// agentID, with the same override semantics as hostsFor.
func (st *fileState) bodyKeysFor(agentID, section string) []string {
	if agentID != "" {
		if keys, ok := st.agentAllowedInBody[agentID][section]; ok {
			return keys
		}
		if !st.agentAllowed(agentID, section) {
			return nil
		}
	}
	return st.allowedInBody[section]
}

// namesFor returns the sorted secret names visible to agentID: the permitted
// globals plus the agent's own keys. The root gets all global keys (agent
// tables excluded).
func (st *fileState) namesFor(agentID string) []string {
	var names []string
	if agentID == "" {
		names = make([]string, 0, len(st.values))
		for k := range st.values {
			names = append(names, k)
		}
	} else {
		seen := make(map[string]bool, len(st.values)+len(st.agentValues[agentID]))
		for k := range st.values {
			section, _, _ := strings.Cut(k, ".")
			if st.agentAllowed(agentID, section) {
				seen[k] = true
			}
		}
		for k := range st.agentValues[agentID] {
			seen[k] = true
		}
		names = make([]string, 0, len(seen))
		for k := range seen {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// valuesFor returns every secret value visible to agentID — the inputs to
// Redact. The root sees all global values; a view sees its permitted globals
// plus its own overrides.
func (st *fileState) valuesFor(agentID string) []string {
	var out []string
	if agentID == "" {
		out = make([]string, 0, len(st.values))
		for _, v := range st.values {
			out = append(out, v)
		}
		return out
	}
	for k, v := range st.values {
		section, _, _ := strings.Cut(k, ".")
		if st.agentAllowed(agentID, section) {
			out = append(out, v)
		}
	}
	for _, v := range st.agentValues[agentID] {
		out = append(out, v)
	}
	return out
}

// source is the shared, lazily-reloading state behind every Store: the last
// good file contents, the file observation they correspond to, the last
// observation that failed (warned about once), and the blocked-path list.
// All fields are guarded by mu. Per-agent views and the root store share one
// source, so a change is visible to every Store on its next read (#1269).
type source struct {
	path string

	mu        sync.RWMutex
	state     *fileState // last good contents; immutable
	loaded    fileStamp  // observation state was loaded from
	warned    fileStamp  // last failed observation (valid when warnedSet)
	warnedSet bool
	blocked   []string
}

// current returns the fileState a read should be answered from, re-reading
// the file first if it changed since the last successful load. One os.Stat
// per call on the unchanged path; no goroutine, timer or watcher (#1269).
func (src *source) current() *fileState {
	src.mu.RLock()
	if cur := statFile(src.path); src.freshLocked(cur) {
		st := src.state
		src.mu.RUnlock()
		return st
	}
	src.mu.RUnlock()

	// Escalate. Re-observe under the write lock: another reader may have
	// reloaded in the meantime, or the file may have changed again.
	src.mu.Lock()
	defer src.mu.Unlock()
	if cur := statFile(src.path); !src.freshLocked(cur) {
		src.reloadLocked(cur)
	}
	return src.state
}

// freshLocked reports whether cur matches the state already in hand: the
// observation the last successful load came from, or the observation already
// warned about (a failed state is not retried until the file changes again).
func (src *source) freshLocked(cur fileStamp) bool {
	return cur.same(src.loaded) || (src.warnedSet && cur.same(src.warned))
}

// reloadLocked swaps in a fresh parse of the file. On failure the last good
// contents stay and exactly one warning is logged per file state: warn once
// for a new observation, stay quiet while it persists, try again as soon as
// the observation changes. The on-disk file is the source of truth — a
// successful reload replaces in-memory mutations, and every mutator caller
// immediately Saves.
func (src *source) reloadLocked(cur fileStamp) {
	st, err := parseFile(src.path)
	if err != nil {
		if !src.warnedSet || !cur.same(src.warned) {
			secretsLog.Warnf("secrets file %s changed but could not be reloaded — keeping the last good contents: %v", src.path, err)
			src.warned, src.warnedSet = cur, true
		}
		return
	}
	src.state = st
	src.loaded = statFile(src.path)
	src.warnedSet = false
}

// mutate applies fn to the current state under the write lock and publishes
// whatever it returns; used by the root store's mutators.
func (src *source) mutate(fn func(st *fileState) *fileState) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.state = fn(src.state)
}

// snapshot returns the current state for rendering outside the lock (Save);
// safe because states are immutable.
func (src *source) snapshot() *fileState {
	src.mu.RLock()
	defer src.mu.RUnlock()
	return src.state
}

// markSaved records the file as it now stands as the loaded observation, so
// the next read does not re-parse the file this process just wrote (which
// could drop a mutation that landed between rendering and writing).
func (src *source) markSaved() {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.loaded = statFile(src.path)
	src.warnedSet = false
}

// blockedSnapshot returns a copy of the blocked-path list, seeded at
// construction and extended only by addBlockedPaths; reloads never touch it.
func (src *source) blockedSnapshot() []string {
	src.mu.RLock()
	defer src.mu.RUnlock()
	return append([]string(nil), src.blocked...)
}

// addBlockedPaths extends the blocked-path list.
func (src *source) addBlockedPaths(paths []string) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.blocked = append(src.blocked, paths...)
}

// parseFile reads and parses the secrets file at path with Load's rules:
// "[section] key = value" flattens to "section.key", [agents.ID] sub-tables
// to per-agent maps, and a section with both allowed_agents and denied_agents
// is rejected. A missing file is an error — Load special-cases the initial
// load of a not-yet-existing file.
func parseFile(path string) (*fileState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}

	st := emptyState()

	var raw map[string]map[string]interface{}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse secrets: %w", err)
	}

	// Flatten: [section] key = value → "section.key" = value
	for section, pairs := range raw {
		if section == "agents" {
			// [agents.ID] sections → per-agent overrides
			for agentID, v := range pairs {
				agentTable, ok := v.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("parse secrets: [agents.%s] must be a table, got %T", agentID, v)
				}
				flattenInto(agentID, agentTable, st)
			}
			continue
		}
		for key, value := range pairs {
			switch v := value.(type) {
			case string:
				st.values[section+"."+key] = v
			case int64:
				st.values[section+"."+key] = strconv.FormatInt(v, 10)
			case []interface{}:
				strs := make([]string, 0, len(v))
				for _, h := range v {
					if hs, ok := h.(string); ok {
						strs = append(strs, hs)
					}
				}
				switch key {
				case "allowed_hosts":
					st.allowedHosts[section] = strs
				case "allowed_agents":
					st.allowedAgents[section] = strs
				case "denied_agents":
					st.deniedAgents[section] = strs
				case "allowed_in_body":
					st.allowedInBody[section] = strs
				}
				// silently skip other array keys
			default:
				// silently skip unknown types
			}
		}
	}

	// Validate: no section may have both allowed_agents and denied_agents
	for section := range st.allowedAgents {
		if _, ok := st.deniedAgents[section]; ok {
			return nil, fmt.Errorf("section [%s] has both allowed_agents and denied_agents — use one or the other", section)
		}
	}

	return st, nil
}

// flattenInto parses one [agents.ID] sub-table and stores its values into
// st.agentValues, st.agentHosts and st.agentAllowedInBody.
func flattenInto(agentID string, table map[string]interface{}, st *fileState) {
	if st.agentValues[agentID] == nil {
		st.agentValues[agentID] = make(map[string]string)
	}
	for section, v := range table {
		subTable, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for key, val := range subTable {
			switch tv := val.(type) {
			case string:
				st.agentValues[agentID][section+"."+key] = tv
			case int64:
				st.agentValues[agentID][section+"."+key] = strconv.FormatInt(tv, 10)
			case []interface{}:
				strs := make([]string, 0, len(tv))
				for _, h := range tv {
					if hs, ok := h.(string); ok {
						strs = append(strs, hs)
					}
				}
				switch key {
				case "allowed_hosts":
					if st.agentHosts[agentID] == nil {
						st.agentHosts[agentID] = make(map[string][]string)
					}
					st.agentHosts[agentID][section] = strs
				case "allowed_in_body":
					if st.agentAllowedInBody[agentID] == nil {
						st.agentAllowedInBody[agentID] = make(map[string][]string)
					}
					st.agentAllowedInBody[agentID][section] = strs
				}
			}
		}
	}
}
