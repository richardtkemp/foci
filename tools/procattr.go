package tools

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"clod/log"
)

// childCredential is set at init time to drop supplementary groups
// (specifically clod-secrets) from exec'd child processes. If the
// primary GID cannot be determined or CAP_SETGID is unavailable,
// this remains nil and children inherit the parent's groups.
var childCredential *syscall.Credential

func init() {
	uid := os.Getuid()
	gid := os.Getgid()

	// If running as root we don't need to drop groups (and the security
	// model doesn't apply — root can read anything).
	if uid == 0 {
		return
	}

	// Look up primary group. os.Getgid() gives us the runtime value;
	// also try user.Current() as a cross-check.
	primaryGID := uint32(gid)
	if u, err := user.Current(); err == nil {
		if g, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			primaryGID = uint32(g)
		}
	}

	cred := &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    primaryGID,
		Groups: []uint32{primaryGID}, // only primary group — drops clod-secrets
	}

	// Probe: try spawning a trivial process with the credential.
	// If CAP_SETGID is not available, setgroups() fails and we should
	// not set the credential (which would break all exec calls).
	probe := exec.Command("true")
	probe.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: cred,
	}
	if err := probe.Run(); err != nil {
		log.Warnf("exec", "cannot drop supplementary groups (CAP_SETGID not available): %v", err)
		log.Warnf("exec", "child processes will inherit parent groups — add AmbientCapabilities=CAP_SETGID to systemd unit")
		return
	}

	childCredential = cred
	log.Debugf("exec", "child credential: uid=%d gid=%d groups=%v", uid, primaryGID, childCredential.Groups)
}

// ChildSysProcAttr returns a SysProcAttr that creates a new process group
// and drops supplementary groups from child processes. If credential setup
// failed at init time, only Setpgid is set.
// Exported so main.go can wire it into the command package.
func ChildSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}

// ChildSysProcAttrSetsid returns a SysProcAttr that creates a new session
// (for background/daemon processes) and drops supplementary groups.
func ChildSysProcAttrSetsid() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setsid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}
