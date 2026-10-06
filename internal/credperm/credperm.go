// Package credperm checks, at startup, that the login credentials delegated
// backends keep on disk are readable by the service user only (#1486).
//
// WHAT THIS CAN AND CANNOT PROTECT. Each backend (Claude Code, Codex,
// opencode) runs as the service user and rewrites its own credentials file when
// it refreshes an OAuth token, so the file must stay owned and writable by that
// user — root ownership (as for secrets.toml or the LD_PRELOAD shim, #1487)
// would break token refresh. That also means an agent, running as the same uid,
// can always read it; nothing here changes that. What IS achievable is keeping
// every OTHER account out: owner = the service user, no group/other permission
// bits, no extended ACL entries. The installer applies exactly that
// (Makefile protect-backend-creds); this package WARNs at startup when a file
// has drifted, giving the verbatim command that fixes it.
//
// Parent directories are deliberately not checked or changed: ~/.claude also
// holds session transcripts that operator tooling reads through group access,
// so tightening it is a separate decision.
package credperm

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"foci/internal/log"
)

var credLog = log.NewComponentLogger("credperm")

// Files are the backend credential files, relative to the service user's home.
// The Makefile's BACKEND_CRED_FILES must list the same paths
// (TestFilesMatchMakefile keeps them in step).
var Files = []string{
	".claude/.credentials.json",       // Claude Code (ccstream)
	".codex/auth.json",                // Codex
	".local/share/opencode/auth.json", // opencode
}

// fileState is what the check needs to know about one file. Split out from the
// filesystem so tests can model owners and ACLs they cannot create.
type fileState struct {
	uid    uint32
	perm   os.FileMode
	hasACL bool
}

// Check logs a WARN for every existing backend credentials file that is
// accessible beyond the service user. Missing files are normal (that backend
// is not logged in) and are skipped silently.
func Check() {
	home, err := os.UserHomeDir()
	if err != nil {
		credLog.Warnf("cannot resolve home dir; backend credential permissions not checked: %v", err)
		return
	}
	userName := strconv.Itoa(os.Geteuid())
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}
	self := uint32(os.Geteuid()) //nolint:gosec // uids are non-negative and fit in uint32
	for _, w := range scan(home, self, userName) {
		credLog.Warnf("%s", w)
	}
}

// scan returns one warning line per credentials file under home that exists
// and is not protected.
func scan(home string, self uint32, userName string) []string {
	var out []string
	for _, rel := range Files {
		path := filepath.Join(home, rel)
		st, err := statFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, fmt.Sprintf("cannot check backend credentials %s: %v", path, err))
			continue
		}
		if problem, fix := assess(path, st, self, userName); problem != "" {
			out = append(out, fmt.Sprintf("backend credentials %s are not protected: %s. Fix: %s", path, problem, fix))
		}
	}
	return out
}

// statFile follows symlinks: what matters is who can read the bytes.
func statFile(path string) (fileState, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileState{}, err
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileState{}, fmt.Errorf("no owner information")
	}
	// An ACL equivalent to the mode bits is not stored, so the attribute
	// existing at all means there are extended entries.
	_, aclErr := unix.Getxattr(path, "system.posix_acl_access", nil)
	return fileState{uid: sys.Uid, perm: fi.Mode().Perm(), hasACL: aclErr == nil}, nil
}

// assess returns a description of what is wrong with st and the verbatim shell
// command that fixes it, or "" when the file is protected. The command is
// meant to be pasted as-is by an operator or agent, so paths are quoted.
func assess(path string, st fileState, self uint32, userName string) (problem, fix string) {
	q := shellQuote(path)
	switch {
	case st.uid != self:
		return fmt.Sprintf("owned by uid %d, not the service user %s (the backend cannot refresh it, and that account can read it)", st.uid, userName),
			fmt.Sprintf("sudo chown %s %s && chmod 600 %s", userName, q, q)
	case st.hasACL:
		return "it has extended ACL entries granting other accounts access",
			fmt.Sprintf("setfacl -b %s && chmod 600 %s", q, q)
	case st.perm&0o077 != 0:
		return fmt.Sprintf("mode %04o lets group/other read or write it", st.perm),
			fmt.Sprintf("chmod 600 %s", q)
	}
	return "", ""
}

func shellQuote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
			continue
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
