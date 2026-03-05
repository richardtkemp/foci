package tools

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"foci/internal/log"
	"foci/internal/secrets"
)

// childCredential is set at init time to drop the foci-secrets
// supplementary group from exec'd child processes while preserving
// all other groups (docker, git, etc.). If the foci-secrets group
// doesn't exist or CAP_SETGID is unavailable, this remains nil.
var childCredential *syscall.Credential

func init() {
	uid := os.Getuid()
	gid := os.Getgid()

	// If running as root we don't need to drop groups (and the security
	// model doesn't apply — root can read anything).
	if uid == 0 {
		return
	}

	// Look up the foci-secrets group. If it doesn't exist, there's
	// nothing to protect against — skip credential setup entirely.
	secretsGrp, err := user.LookupGroup(secrets.SecurityGroupName)
	if err != nil {
		log.Debugf("exec", "group %q not found — skipping child credential setup", secrets.SecurityGroupName)
		return
	}
	secretsGID, err := strconv.ParseUint(secretsGrp.Gid, 10, 32)
	if err != nil {
		return
	}

	// Get current supplementary groups
	currentGroups, err := syscall.Getgroups()
	if err != nil {
		log.Warnf("exec", "cannot read supplementary groups: %v", err)
		return
	}

	// Build filtered list: all groups EXCEPT foci-secrets
	// #nosec G115 - GID/UID conversions are safe, values are always non-negative and within uint32 range
	var filteredGroups []uint32
	found := false
	for _, g := range currentGroups {
		if uint64(g) == secretsGID {
			found = true
			continue // drop foci-secrets
		}
		filteredGroups = append(filteredGroups, uint32(g))
	}

	if !found {
		// Process doesn't have foci-secrets — nothing to drop
		log.Debugf("exec", "process does not have %s group — skipping child credential setup", secrets.SecurityGroupName)
		return
	}

	// Look up primary GID
	primaryGID := uint32(gid)
	if u, err := user.Current(); err == nil {
		if g, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			primaryGID = uint32(g)
		}
	}

	cred := &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    primaryGID,
		Groups: filteredGroups, // all groups except foci-secrets
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
		log.Warnf("exec", "cannot drop %s group (CAP_SETGID not available): %v", secrets.SecurityGroupName, err)
		log.Warnf("exec", "child processes will inherit parent groups — add AmbientCapabilities=CAP_SETGID to systemd unit")
		return
	}

	childCredential = cred
	log.Debugf("exec", "child credential: uid=%d gid=%d groups=%v (dropped %s gid %d)",
		uid, primaryGID, filteredGroups, secrets.SecurityGroupName, secretsGID)
}

// ChildSysProcAttr returns a SysProcAttr that creates a new process group
// and drops the foci-secrets supplementary group from child processes.
// All other groups are preserved. If credential setup failed at init
// time, only Setpgid is set.
// Exported so main.go can wire it into the command package.
func ChildSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}

// ChildSysProcAttrSetsid returns a SysProcAttr that creates a new session
// (for background/daemon processes) and drops the foci-secrets group.
func ChildSysProcAttrSetsid() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setsid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}
