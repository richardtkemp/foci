package secrets

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"github.com/BurntSushi/toml"
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

// Store holds secrets loaded from secrets.toml.
// Values are stored as flat keys: "anthropic.setup_token", "custom.github_token", etc.
type Store struct {
	path          string
	values        map[string]string
	allowedHosts  map[string][]string            // section name → allowed hosts
	allowedAgents map[string][]string            // section name → agent whitelist
	deniedAgents       map[string][]string            // section name → agent blacklist
	allowedInBody      map[string][]string            // section name → key names allowed in request body
	blockedPaths       []string
	agentValues        map[string]map[string]string   // agent ID → flat key → value
	agentHosts         map[string]map[string][]string // agent ID → section → allowed hosts
	agentAllowedInBody map[string]map[string][]string // agent ID → section → key names allowed in body
}

// Load reads secrets from a TOML file. Returns an empty store (not error) if the file doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{
		path:          path,
		values:        make(map[string]string),
		allowedHosts:  make(map[string][]string),
		allowedAgents: make(map[string][]string),
		deniedAgents:  make(map[string][]string),
		allowedInBody: make(map[string][]string),
		blockedPaths:  append([]string{}, defaultBlockedPaths...),
	}

	// Add the secrets file itself to blocked paths
	s.blockedPaths = append(s.blockedPaths, path)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}

	var raw map[string]map[string]interface{}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse secrets: %w", err)
	}

	// Flatten: [section] key = value → "section.key" = value
	for section, pairs := range raw {
		if section == "agents" {
			// [agents.ID] sections → per-agent overrides
			s.agentValues = make(map[string]map[string]string)
			s.agentHosts = make(map[string]map[string][]string)
			s.agentAllowedInBody = make(map[string]map[string][]string)
			for agentID, v := range pairs {
				agentTable, ok := v.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("parse secrets: [agents.%s] must be a table, got %T", agentID, v)
				}
				flattenInto(agentID, agentTable, s)
			}
			continue
		}
		for key, value := range pairs {
			switch v := value.(type) {
			case string:
				s.values[section+"."+key] = v
			case int64:
				s.values[section+"."+key] = strconv.FormatInt(v, 10)
			case []interface{}:
				strs := make([]string, 0, len(v))
				for _, h := range v {
					if hs, ok := h.(string); ok {
						strs = append(strs, hs)
					}
				}
				switch key {
				case "allowed_hosts":
					s.allowedHosts[section] = strs
				case "allowed_agents":
					s.allowedAgents[section] = strs
				case "denied_agents":
					s.deniedAgents[section] = strs
				case "allowed_in_body":
					s.allowedInBody[section] = strs
				}
				// silently skip other array keys
			default:
				// silently skip unknown types
			}
		}
	}

	// Validate: no section may have both allowed_agents and denied_agents
	for section := range s.allowedAgents {
		if _, ok := s.deniedAgents[section]; ok {
			return nil, fmt.Errorf("section [%s] has both allowed_agents and denied_agents — use one or the other", section)
		}
	}

	return s, nil
}

// flattenInto parses one [agents.ID] sub-table and stores its values
// into s.agentValues and s.agentHosts.
func flattenInto(agentID string, table map[string]interface{}, s *Store) {
	if s.agentValues[agentID] == nil {
		s.agentValues[agentID] = make(map[string]string)
	}
	for section, v := range table {
		subTable, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		for key, val := range subTable {
			switch tv := val.(type) {
			case string:
				s.agentValues[agentID][section+"."+key] = tv
			case int64:
				s.agentValues[agentID][section+"."+key] = strconv.FormatInt(tv, 10)
			case []interface{}:
				strs := make([]string, 0, len(tv))
				for _, h := range tv {
					if hs, ok := h.(string); ok {
						strs = append(strs, hs)
					}
				}
				switch key {
				case "allowed_hosts":
					if s.agentHosts[agentID] == nil {
						s.agentHosts[agentID] = make(map[string][]string)
					}
					s.agentHosts[agentID][section] = strs
				case "allowed_in_body":
					if s.agentAllowedInBody[agentID] == nil {
						s.agentAllowedInBody[agentID] = make(map[string][]string)
					}
					s.agentAllowedInBody[agentID][section] = strs
				}
			}
		}
	}
}

// ForAgent returns a new Store scoped to the given agent ID.
// Agent-specific values overlay globals; keys not overridden fall back to globals.
// Global sections with allowed_agents/denied_agents are filtered before overlay.
// The returned Store has no path (cannot Save) and no agentValues (doesn't nest further).
func (s *Store) ForAgent(agentID string) *Store {
	// 1. Copy globals
	merged := make(map[string]string, len(s.values))
	for k, v := range s.values {
		merged[k] = v
	}
	// 2. Filter globals by agent restrictions
	for k := range merged {
		section := k[:strings.IndexByte(k, '.')]
		if !s.agentAllowed(agentID, section) {
			delete(merged, k)
		}
	}
	// 3. Overlay agent-specific (always allowed)
	if s.agentValues != nil {
		for k, v := range s.agentValues[agentID] {
			merged[k] = v
		}
	}

	// Same for hosts: filter globals, then overlay agent hosts
	mergedHosts := make(map[string][]string, len(s.allowedHosts))
	for k, v := range s.allowedHosts {
		if s.agentAllowed(agentID, k) {
			mergedHosts[k] = v
		}
	}
	if s.agentHosts != nil {
		for k, v := range s.agentHosts[agentID] {
			mergedHosts[k] = v
		}
	}

	// Same for allowedInBody: filter globals, overlay agent-specific
	mergedAllowedInBody := make(map[string][]string, len(s.allowedInBody))
	for k, v := range s.allowedInBody {
		if s.agentAllowed(agentID, k) {
			mergedAllowedInBody[k] = v
		}
	}
	if s.agentAllowedInBody != nil {
		for k, v := range s.agentAllowedInBody[agentID] {
			mergedAllowedInBody[k] = v
		}
	}

	return &Store{
		values:        merged,
		allowedHosts:  mergedHosts,
		allowedInBody: mergedAllowedInBody,
		blockedPaths:  s.blockedPaths,
	}
}

// agentAllowed checks whether agentID is permitted to access the given section
// based on allowed_agents/denied_agents rules. No restrictions means allowed.
func (s *Store) agentAllowed(agentID, section string) bool {
	if allowed, ok := s.allowedAgents[section]; ok {
		for _, a := range allowed {
			if a == agentID {
				return true
			}
		}
		return false
	}
	if denied, ok := s.deniedAgents[section]; ok {
		for _, a := range denied {
			if a == agentID {
				return false
			}
		}
	}
	return true
}

// HasAgentRestrictions reports whether any section has allowed_agents or denied_agents.
func (s *Store) HasAgentRestrictions() bool {
	return len(s.allowedAgents) > 0 || len(s.deniedAgents) > 0
}

// Get returns a secret value by its flat key (e.g. "anthropic.setup_token").
func (s *Store) Get(name string) (string, bool) {
	v, ok := s.values[name]
	return v, ok
}

// Names returns all secret names (keys), sorted.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.values))
	for k := range s.values {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Set adds or updates a secret value by its flat key (e.g. "section.key").
func (s *Store) Set(name, value string) {
	s.values[name] = value
}

// Remove deletes a secret by its flat key. Returns true if found.
func (s *Store) Remove(name string) bool {
	if _, ok := s.values[name]; !ok {
		return false
	}
	delete(s.values, name)
	return true
}

// Save writes the current secrets back to the TOML file.
func (s *Store) Save() error {
	sections := flatKeysToSections(s.values)

	var buf strings.Builder

	// Write global sections
	secNames := sortedKeyUnion(keysOf(sections), keysOf(s.allowedHosts), keysOf(s.allowedAgents), keysOf(s.deniedAgents), keysOf(s.allowedInBody))
	for i, sec := range secNames {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[%s]\n", sec)
		writeKeyValues(&buf, sections[sec])
		writeStringArrayField(&buf, "allowed_hosts", s.allowedHosts[sec])
		writeStringArrayField(&buf, "allowed_agents", s.allowedAgents[sec])
		writeStringArrayField(&buf, "denied_agents", s.deniedAgents[sec])
		writeStringArrayField(&buf, "allowed_in_body", s.allowedInBody[sec])
	}

	// Write [agents.*] sections
	agentIDs := sortedKeyUnion(keysOf(s.agentValues), keysOf(s.agentHosts), keysOf(s.agentAllowedInBody))
	for _, agentID := range agentIDs {
		agentSections := flatKeysToSections(s.agentValues[agentID])
		var agentHosts map[string][]string
		if s.agentHosts != nil {
			agentHosts = s.agentHosts[agentID]
		}
		var agentBody map[string][]string
		if s.agentAllowedInBody != nil {
			agentBody = s.agentAllowedInBody[agentID]
		}
		subSecs := sortedKeyUnion(keysOf(agentSections), keysOf(agentHosts), keysOf(agentBody))
		for _, sec := range subSecs {
			buf.WriteByte('\n')
			fmt.Fprintf(&buf, "[agents.%s.%s]\n", agentID, sec)
			writeKeyValues(&buf, agentSections[sec])
			writeStringArrayField(&buf, "allowed_hosts", agentHosts[sec])
			writeStringArrayField(&buf, "allowed_in_body", agentBody[sec])
		}
	}

	return os.WriteFile(s.path, []byte(buf.String()), 0600)
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

