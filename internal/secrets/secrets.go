package secrets

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

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
	deniedAgents  map[string][]string            // section name → agent blacklist
	blockedPaths  []string
	agentValues   map[string]map[string]string   // agent ID → flat key → value
	agentHosts    map[string]map[string][]string // agent ID → section → allowed hosts
}

// Load reads secrets from a TOML file. Returns an empty store (not error) if the file doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{
		path:          path,
		values:        make(map[string]string),
		allowedHosts:  make(map[string][]string),
		allowedAgents: make(map[string][]string),
		deniedAgents:  make(map[string][]string),
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
			for agentID, v := range pairs {
				agentTable, ok := v.(map[string]interface{})
				if !ok {
					continue
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
				if key == "allowed_hosts" {
					hosts := make([]string, 0, len(tv))
					for _, h := range tv {
						if hs, ok := h.(string); ok {
							hosts = append(hosts, hs)
						}
					}
					if s.agentHosts[agentID] == nil {
						s.agentHosts[agentID] = make(map[string][]string)
					}
					s.agentHosts[agentID][section] = hosts
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

	return &Store{
		values:       merged,
		allowedHosts: mergedHosts,
		blockedPaths: s.blockedPaths,
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
	secNames := sortedKeyUnion(keysOf(sections), keysOf(s.allowedHosts), keysOf(s.allowedAgents), keysOf(s.deniedAgents))
	for i, sec := range secNames {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[%s]\n", sec)
		writeKeyValues(&buf, sections[sec])
		writeStringArrayField(&buf, "allowed_hosts", s.allowedHosts[sec])
		writeStringArrayField(&buf, "allowed_agents", s.allowedAgents[sec])
		writeStringArrayField(&buf, "denied_agents", s.deniedAgents[sec])
	}

	// Write [agents.*] sections
	agentIDs := sortedKeyUnion(keysOf(s.agentValues), keysOf(s.agentHosts))
	for _, agentID := range agentIDs {
		agentSections := flatKeysToSections(s.agentValues[agentID])
		var agentHosts map[string][]string
		if s.agentHosts != nil {
			agentHosts = s.agentHosts[agentID]
		}
		subSecs := sortedKeyUnion(keysOf(agentSections), keysOf(agentHosts))
		for _, sec := range subSecs {
			buf.WriteByte('\n')
			fmt.Fprintf(&buf, "[agents.%s.%s]\n", agentID, sec)
			writeKeyValues(&buf, agentSections[sec])
			writeStringArrayField(&buf, "allowed_hosts", agentHosts[sec])
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

var templateRe = regexp.MustCompile(`\{\{secret:([a-zA-Z0-9_.\-]+)\}\}`)

// FindSecretRefs returns all secret names referenced by {{secret:NAME}} templates in text.
// Returns nil if no templates are found.
func FindSecretRefs(text string) []string {
	matches := templateRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var names []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// AllowedHosts returns the allowed_hosts list for the section of the given
// secret name. For example, "anthropic.setup_token" returns allowedHosts["anthropic"].
// Returns nil if no allowed_hosts are configured for that section.
func (s *Store) AllowedHosts(name string) []string {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) < 2 {
		return nil
	}
	return s.allowedHosts[parts[0]]
}

// SectionAllowedHosts returns the allowed_hosts for a section name directly
// (e.g. "anthropic", "custom"). Returns nil if no hosts are configured.
func (s *Store) SectionAllowedHosts(section string) []string {
	return s.allowedHosts[section]
}

// SetAllowedHosts replaces the allowed_hosts list for a section.
// Pass nil or empty to remove all allowed_hosts for the section.
func (s *Store) SetAllowedHosts(section string, hosts []string) {
	if len(hosts) == 0 {
		delete(s.allowedHosts, section)
	} else {
		s.allowedHosts[section] = hosts
	}
}

// AddAllowedHost adds a host to the section's allowed_hosts list.
// Host is normalized to lowercase. No-op if already present.
func (s *Store) AddAllowedHost(section, host string) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return
	}
	for _, h := range s.allowedHosts[section] {
		if strings.EqualFold(h, host) {
			return // already present
		}
	}
	s.allowedHosts[section] = append(s.allowedHosts[section], host)
}

// RemoveAllowedHost removes a host from the section's allowed_hosts list.
// Case-insensitive comparison. Returns true if found and removed.
func (s *Store) RemoveAllowedHost(section, host string) bool {
	hosts := s.allowedHosts[section]
	for i, h := range hosts {
		if strings.EqualFold(h, host) {
			s.allowedHosts[section] = append(hosts[:i], hosts[i+1:]...)
			if len(s.allowedHosts[section]) == 0 {
				delete(s.allowedHosts, section)
			}
			return true
		}
	}
	return false
}

// CheckHostAllowed verifies that the target URL's host is in the allowed_hosts
// list for the given secret. Returns an error if:
// - the secret has no allowed_hosts configured
// - the URL cannot be parsed
// - the host is not in the allowed list
//
// Uses url.Parse().Hostname() which strips userinfo and port, defending against
// userinfo injection attacks (e.g. https://api.example.com@evil.com/steal).
// Host comparison is case-insensitive per RFC 4343.
func (s *Store) CheckHostAllowed(secretName, targetURL string) error {
	hosts := s.AllowedHosts(secretName)
	if len(hosts) == 0 {
		return fmt.Errorf("secret %q has no allowed_hosts configured — add allowed_hosts to the [%s] section in secrets.toml",
			secretName, strings.SplitN(secretName, ".", 2)[0])
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", targetURL, err)
	}

	hostname := parsed.Hostname() // strips userinfo and port
	for _, allowed := range hosts {
		if strings.EqualFold(hostname, allowed) {
			return nil
		}
	}

	return fmt.Errorf("host %q not in allowed_hosts for secret %q (allowed: %v)", hostname, secretName, hosts)
}

// Resolve expands all {{secret:NAME}} templates in text with their values.
// Returns an error if any template references an unknown secret.
func (s *Store) Resolve(text string) (string, error) {
	var resolveErr error

	result := templateRe.ReplaceAllStringFunc(text, func(match string) string {
		submatch := templateRe.FindStringSubmatch(match)
		name := submatch[1]
		val, ok := s.values[name]
		if !ok {
			resolveErr = fmt.Errorf("unknown secret: %q", name)
			return match // leave unresolved
		}
		return val
	})

	if resolveErr != nil {
		return "", resolveErr
	}
	return result, nil
}

// Redact replaces any occurrence of a secret value in text with [REDACTED].
// Longer values are checked first to avoid partial matches.
func (s *Store) Redact(text string) string {
	if len(s.values) == 0 {
		return text
	}

	// Sort values by length descending so longer secrets are redacted first
	vals := make([]string, 0, len(s.values))
	for _, v := range s.values {
		if len(v) >= 4 { // don't redact very short values that would cause false positives
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		return len(vals[i]) > len(vals[j])
	})

	for _, v := range vals {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
	}
	return text
}

// AddBlockedPaths adds additional paths to the blocklist.
func (s *Store) AddBlockedPaths(paths []string) {
	s.blockedPaths = append(s.blockedPaths, paths...)
}

// IsBlockedPath returns true if the given path matches any blocked path.
// Checks both exact substring match and basename match.
func (s *Store) IsBlockedPath(path string) bool {
	for _, blocked := range s.blockedPaths {
		if strings.Contains(path, blocked) {
			return true
		}
	}
	return false
}

// IsBlockedCommand checks if a shell command references any blocked paths.
func (s *Store) IsBlockedCommand(cmd string) bool {
	for _, blocked := range s.blockedPaths {
		if strings.Contains(cmd, blocked) {
			return true
		}
	}
	return false
}

// SecurityGroupName is the OS group that protects secrets.toml.
// Tests may override securityGroupName for isolation.
const SecurityGroupName = "foci-secrets"

// securityGroupName is the group name used by CheckSecurity.
// Matches SecurityGroupName by default; tests override for isolation.
var securityGroupName = SecurityGroupName

// CheckSecurity verifies the OS-level protection of secrets.toml.
// Returns a list of warning messages for any issues found.
// Does not prevent startup — issues are advisory only.
func (s *Store) CheckSecurity() []string {
	if s.path == "" {
		return nil
	}

	var warnings []string

	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		// No secrets file — nothing to protect
		return nil
	}
	if err != nil {
		return []string{fmt.Sprintf("cannot stat %s: %v", s.path, err)}
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return []string{"cannot read file ownership (unsupported platform)"}
	}

	// Check owner is root (uid 0)
	if stat.Uid != 0 {
		warnings = append(warnings,
			fmt.Sprintf("secrets.toml owner is uid %d, expected root (uid 0) — run: sudo chown root:%s %s", stat.Uid, SecurityGroupName, s.path))
	}

	// Check group is foci-secrets
	grp, err := user.LookupGroup(securityGroupName)
	if err != nil {
		warnings = append(warnings,
			fmt.Sprintf("group %q not found — run: sudo groupadd %s", securityGroupName, securityGroupName))
	} else {
		expectedGID, _ := strconv.ParseUint(grp.Gid, 10, 32)
		if uint64(stat.Gid) != expectedGID {
			warnings = append(warnings,
				fmt.Sprintf("secrets.toml group is gid %d, expected %s (gid %s) — run: sudo chown root:%s %s",
					stat.Gid, SecurityGroupName, grp.Gid, SecurityGroupName, s.path))
		}
	}

	// Check permissions are 0660
	mode := info.Mode().Perm()
	if mode != 0660 {
		warnings = append(warnings,
			fmt.Sprintf("secrets.toml permissions are %04o, expected 0660 — run: sudo chmod 0660 %s", mode, s.path))
	}

	// Check process has foci-secrets in supplementary groups
	if grp != nil {
		expectedGID, _ := strconv.ParseUint(grp.Gid, 10, 32)
		gids, err := syscall.Getgroups()
		if err == nil {
			found := false
			for _, g := range gids {
				if uint64(g) == expectedGID { // #nosec G115 - GID is always non-negative
					found = true
					break
				}
			}
			if !found {
				warnings = append(warnings,
					fmt.Sprintf("process does not have %s in supplementary groups — add SupplementaryGroups=%s to systemd unit",
						securityGroupName, securityGroupName))
			}
		}
	}

	return warnings
}
