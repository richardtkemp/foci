package secrets

import (
	"fmt"
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

// Default paths that the exec tool should refuse to read.
var defaultBlockedPaths = []string{
	"secrets.toml",
	"/proc/self/environ",
}

// Store holds secrets loaded from secrets.toml.
// Values are stored as flat keys: "anthropic.token", "custom.github_token", etc.
type Store struct {
	path         string
	values       map[string]string
	allowedHosts map[string][]string // section name → allowed hosts
	blockedPaths []string
}

// Load reads secrets from a TOML file. Returns an empty store (not error) if the file doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{
		path:         path,
		values:       make(map[string]string),
		allowedHosts: make(map[string][]string),
		blockedPaths: append([]string{}, defaultBlockedPaths...),
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
		for key, value := range pairs {
			switch v := value.(type) {
			case string:
				s.values[section+"."+key] = v
			case []interface{}:
				if key == "allowed_hosts" {
					hosts := make([]string, 0, len(v))
					for _, h := range v {
						if hs, ok := h.(string); ok {
							hosts = append(hosts, hs)
						}
					}
					s.allowedHosts[section] = hosts
				}
				// silently skip other array keys
			default:
				// silently skip unknown types
			}
		}
	}

	return s, nil
}

// Get returns a secret value by its flat key (e.g. "anthropic.token").
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
	// Rebuild section map from flat keys
	sections := make(map[string]map[string]string)
	for flat, val := range s.values {
		parts := strings.SplitN(flat, ".", 2)
		if len(parts) != 2 {
			continue
		}
		sec, key := parts[0], parts[1]
		if sections[sec] == nil {
			sections[sec] = make(map[string]string)
		}
		sections[sec][key] = val
	}

	// Collect all sections (values + allowedHosts may have different keys)
	allSections := make(map[string]bool)
	for sec := range sections {
		allSections[sec] = true
	}
	for sec := range s.allowedHosts {
		allSections[sec] = true
	}

	var buf strings.Builder
	// Sort sections for deterministic output
	secNames := make([]string, 0, len(allSections))
	for sec := range allSections {
		secNames = append(secNames, sec)
	}
	sort.Strings(secNames)

	for i, sec := range secNames {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[%s]\n", sec)
		if pairs, ok := sections[sec]; ok {
			keys := make([]string, 0, len(pairs))
			for k := range pairs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&buf, "%s = %q\n", k, pairs[k])
			}
		}
		if hosts, ok := s.allowedHosts[sec]; ok && len(hosts) > 0 {
			buf.WriteString("allowed_hosts = [")
			for j, h := range hosts {
				if j > 0 {
					buf.WriteString(", ")
				}
				fmt.Fprintf(&buf, "%q", h)
			}
			buf.WriteString("]\n")
		}
	}

	return os.WriteFile(s.path, []byte(buf.String()), 0600)
}

var templateRe = regexp.MustCompile(`\{\{secret:([a-zA-Z0-9_.]+)\}\}`)

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
// secret name. For example, "anthropic.token" returns allowedHosts["anthropic"].
// Returns nil if no allowed_hosts are configured for that section.
func (s *Store) AllowedHosts(name string) []string {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) < 2 {
		return nil
	}
	return s.allowedHosts[parts[0]]
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
const SecurityGroupName = "clod-secrets"

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
			fmt.Sprintf("secrets.toml owner is uid %d, expected root (uid 0) — run: sudo chown root:clod-secrets %s", stat.Uid, s.path))
	}

	// Check group is clod-secrets
	grp, err := user.LookupGroup(SecurityGroupName)
	if err != nil {
		warnings = append(warnings,
			fmt.Sprintf("group %q not found — run: sudo groupadd %s", SecurityGroupName, SecurityGroupName))
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

	// Check process has clod-secrets in supplementary groups
	if grp != nil {
		expectedGID, _ := strconv.ParseUint(grp.Gid, 10, 32)
		gids, err := syscall.Getgroups()
		if err == nil {
			found := false
			for _, g := range gids {
				if uint64(g) == expectedGID {
					found = true
					break
				}
			}
			if !found {
				warnings = append(warnings,
					fmt.Sprintf("process does not have %s in supplementary groups — add SupplementaryGroups=%s to systemd unit",
						SecurityGroupName, SecurityGroupName))
			}
		}
	}

	return warnings
}
