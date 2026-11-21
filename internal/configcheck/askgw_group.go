package configcheck

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"foci/internal/config"
)

// askgwGroupDeps is the outside world the askgw group check reads, injected
// so its tests never depend on the host's users and groups.
type askgwGroupDeps struct {
	euid        int
	ownerUID    func(path string) (int, error)
	lookupID    func(uid string) (*user.User, error)
	lookupGroup func(name string) (*user.Group, error)
	groupIDs    func(u *user.User) ([]string, error)
}

func prodAskgwGroupDeps(euid int) askgwGroupDeps {
	return askgwGroupDeps{
		euid:        euid,
		ownerUID:    pathOwnerUID,
		lookupID:    user.LookupId,
		lookupGroup: user.LookupGroup,
		groupIDs:    (*user.User).GroupIds,
	}
}

func pathOwnerUID(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no owner information")
	}
	return int(st.Uid), nil
}

// askgwGroupConcern returns the warning for an enabled askgw whose socket
// group the gateway user cannot set (#3237), or "" when there is none. The
// gateway then serves the socket owner-only (0600) instead of failing, so
// this is a warning: the check names the surprise before the restart.
//
// The gateway user is the user running the check, except as root (the Linux
// deploy gate): there it is the owner of the config file's directory, which
// the install gives to the service user. A root-owned directory means a root
// gateway (which can chown to any group) or an unknown one: no finding.
func askgwGroupConcern(cfg *config.Config, configPath string, d askgwGroupDeps) string {
	if !cfg.Askgw.Enabled {
		return ""
	}
	name := cfg.Askgw.ResolvedGroup()
	uid := d.euid
	if uid == 0 {
		owner, err := d.ownerUID(filepath.Dir(configPath))
		if err != nil || owner == 0 {
			return ""
		}
		uid = owner
	}
	u, err := d.lookupID(strconv.Itoa(uid))
	if err != nil {
		return fmt.Sprintf("[askgw] group %q not checked: cannot look up the gateway user (uid %d): %v", name, uid, err)
	}
	g, err := d.lookupGroup(name)
	if err != nil {
		return fmt.Sprintf("[askgw] group %q does not exist; the gateway will serve the askgw socket owner-only (mode 0600): only %s and root can connect", name, u.Username)
	}
	if u.Gid == g.Gid {
		return ""
	}
	ids, err := d.groupIDs(u)
	if err != nil {
		return fmt.Sprintf("[askgw] group %q not checked: cannot list the groups of the gateway user %s: %v", name, u.Username, err)
	}
	if slices.Contains(ids, g.Gid) {
		return ""
	}
	return fmt.Sprintf("[askgw] group %q (gid %s): the gateway user %s is not a member, so it cannot give the socket that group; the gateway will serve the askgw socket owner-only (mode 0600): only %s and root can connect", name, g.Gid, u.Username, u.Username)
}
