package config

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"foci/internal/procx"
)

// Config file protection (#479). foci-gw and every agent process run as the
// same user, so a config file that user owns is a file every agent can
// rewrite. The protected shape is root:foci-secrets 0664: everyone can read
// it, only root and members of foci-secrets can write it, and foci-gw is the
// only process holding foci-secrets (procx.Setup strips it from every child).
// The gateway refuses to load a file in any other shape (fail closed), and
// every gateway write edits the file in place (WriteConfigFile) so the shape
// survives the gateway's own edits.

// ProtectedConfigMode is the exact mode a protected config file has.
const ProtectedConfigMode os.FileMode = 0o664

// ConfigProtectionEnforced reports whether this platform has the
// root:foci-secrets model at all. Off Linux (macOS) foci runs as the login
// user with no group drop for its children (docs/SECRETS.md "macOS"), so no
// file shape can keep an agent out, and the check is not applied.
func ConfigProtectionEnforced() bool { return runtime.GOOS == "linux" }

// CheckConfigProtected reports whether the config file at path is protected
// (owner uid 0, group foci-secrets, mode exactly 0664). The error names what
// is wrong and the exact command that fixes it.
func CheckConfigProtected(path string) error {
	grp, err := user.LookupGroup(procx.SecurityGroupName)
	if err != nil {
		return fmt.Errorf("config file %s is not protected: group %q does not exist (run: sudo groupadd %s && %s)",
			path, procx.SecurityGroupName, procx.SecurityGroupName, ProtectConfigCommand(path))
	}
	gid, err := strconv.ParseUint(grp.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("config file %s: group %q has a non-numeric gid %q", path, procx.SecurityGroupName, grp.Gid)
	}
	return checkConfigProtection(path, 0, uint32(gid))
}

// checkConfigProtection is CheckConfigProtected with the wanted owner and
// group injected, so tests can judge real files without root.
func checkConfigProtection(path string, wantUID, wantGID uint32) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("config file %s: cannot read its owner on this platform", path)
	}
	var problems []string
	if st.Uid != wantUID {
		problems = append(problems, fmt.Sprintf("owner is uid %d, want uid %d (root)", st.Uid, wantUID))
	}
	if st.Gid != wantGID {
		problems = append(problems, fmt.Sprintf("group is gid %d, want gid %d (%s)", st.Gid, wantGID, procx.SecurityGroupName))
	}
	if rawPerm(st) != uint32(ProtectedConfigMode) {
		problems = append(problems, fmt.Sprintf("mode is %04o, want %04o", rawPerm(st), ProtectedConfigMode))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("config file %s is not protected: %s. Agents could rewrite it. Fix: %s",
		path, strings.Join(problems, "; "), ProtectConfigCommand(path))
}

// rawPerm renders the permission bits including setuid/setgid/sticky.
func rawPerm(st *syscall.Stat_t) uint32 { return uint32(st.Mode) & 0o7777 }

// ProtectConfigCommand is the exact command that protects the config file.
func ProtectConfigCommand(path string) string {
	return fmt.Sprintf("sudo chown root:%s %s && sudo chmod %04o %s", procx.SecurityGroupName, path, ProtectedConfigMode, path)
}
