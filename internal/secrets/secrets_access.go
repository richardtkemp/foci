package secrets

import (
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

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

// IsAllowedInBody reports whether the named secret (section.key) is permitted
// in HTTP request bodies. Returns false by default unless the key is listed in
// allowed_in_body for its section.
func (s *Store) IsAllowedInBody(name string) bool {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) < 2 {
		return false
	}
	section, key := parts[0], parts[1]
	for _, k := range s.allowedInBody[section] {
		if k == key {
			return true
		}
	}
	return false
}

// SectionAllowedInBody returns the allowed_in_body list for a section name
// (e.g. "custom"). Returns nil if none are configured.
func (s *Store) SectionAllowedInBody(section string) []string {
	return s.allowedInBody[section]
}

// SetAllowedInBody replaces the allowed_in_body list for a section.
// Pass nil or empty to remove all allowed_in_body for the section.
func (s *Store) SetAllowedInBody(section string, keys []string) {
	if len(keys) == 0 {
		delete(s.allowedInBody, section)
	} else {
		s.allowedInBody[section] = keys
	}
}

// AddAllowedInBody adds a key to the section's allowed_in_body list.
// No-op if already present.
func (s *Store) AddAllowedInBody(section, key string) {
	for _, k := range s.allowedInBody[section] {
		if k == key {
			return
		}
	}
	s.allowedInBody[section] = append(s.allowedInBody[section], key)
}

// RemoveAllowedInBody removes a key from the section's allowed_in_body list.
// Returns true if found and removed.
func (s *Store) RemoveAllowedInBody(section, key string) bool {
	keys := s.allowedInBody[section]
	for i, k := range keys {
		if k == key {
			s.allowedInBody[section] = append(keys[:i], keys[i+1:]...)
			if len(s.allowedInBody[section]) == 0 {
				delete(s.allowedInBody, section)
			}
			return true
		}
	}
	return false
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

// containsBlockedRef returns true if text contains any blocked path substring.
func (s *Store) containsBlockedRef(text string) bool {
	for _, blocked := range s.blockedPaths {
		if strings.Contains(text, blocked) {
			return true
		}
	}
	return false
}

// CanonicalPath returns an absolute, symlink-resolved form of path. For a path
// that does not exist yet, it resolves the deepest existing ancestor and
// re-appends the remaining components, so a symlinked parent directory is still
// followed. It is best-effort: on any error it falls back to filepath.Abs (or
// the input). This is the canonical form used for security path comparisons —
// both the blocked-path check here and the file-tool containment checks.
func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir := abs
	var tail []string
	for {
		parent := filepath.Dir(dir)
		tail = append(tail, filepath.Base(dir))
		if parent == dir {
			return abs // reached the root without finding an existing ancestor
		}
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
	}
}

// IsBlockedPath reports whether path refers to a protected file. Matching is
// path-canonical (symlinks resolved, absolute), NOT substring: absolute blocked
// entries match by exact path or directory prefix; relative entries (e.g.
// "secrets.toml", ".ssh/id_rsa") match by component-aligned path suffix. This
// rejects both substring false positives ("mysecrets.toml") and the symlink
// bypass — a symlink whose target is the secrets file resolves to the canonical
// path and is caught.
func (s *Store) IsBlockedPath(path string) bool {
	target := CanonicalPath(path)
	sep := string(filepath.Separator)
	for _, blocked := range s.blockedPaths {
		if filepath.IsAbs(blocked) {
			b := CanonicalPath(blocked)
			if target == b || strings.HasPrefix(target, b+sep) {
				return true
			}
			continue
		}
		// Relative entry: match as a whole trailing path-component sequence,
		// so ".aws/credentials" matches "/home/u/.aws/credentials" but
		// "secrets.toml" does not match "/home/u/mysecrets.toml".
		r := filepath.Clean(blocked)
		if strings.HasSuffix(target, sep+r) {
			return true
		}
	}
	return false
}

// IsBlockedCommand checks if a shell command references any blocked paths. This
// stays a substring scan: it inspects an unparsed command line, not a resolved
// filesystem path, and is advisory only (the OS group-drop and the in-process
// path checks are the real boundaries).
func (s *Store) IsBlockedCommand(cmd string) bool {
	return s.containsBlockedRef(cmd)
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
				// #nosec G115 - GID values are always non-negative and within uint64 range
				if uint64(g) == expectedGID {
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
