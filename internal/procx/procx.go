// Package procx is the single allowed entry point for spawning external
// processes from foci-gw. Direct use of os/exec.Command or
// os/exec.CommandContext outside this file is banned by the forbidigo
// linter (.golangci.yml).
//
// # Why this exists
//
// foci-gw runs as the `foci` UNIX user with the `foci-secrets`
// supplementary group, which grants read access to the foci secrets file
// (typically /home/foci/config/secrets.toml, mode 0660 root:foci-secrets).
// Any child process inherits the parent's supplementary groups unless we
// deliberately strip them. Without the strip, every subprocess foci spawns
// — including delegated Claude Code agents, the Bash builtin, MCP servers,
// pandoc/ssconvert, TTS, etc. — would be able to read the secrets file.
//
// # API
//
//   - Setup() probes whether the process holds CAP_SETGID and stashes a
//     Credential that drops the foci-secrets group. Call once at startup
//     from foci-gw; other binaries should not call it.
//   - Spawn / SpawnSetsid construct an *exec.Cmd with the credential
//     applied and a process-group / session marker set so signal cleanup
//     works.
//
// Callers must always go through Spawn / SpawnSetsid. If a future code
// path has a legitimate need for raw exec.Command (e.g., the CAP_SETGID
// probe inside this file), add it to the forbidigo exclude list in
// .golangci.yml with a comment explaining why.
//
// # Living outside internal/tools
//
// The helper lives in its own leaf package (rather than internal/tools)
// because several packages that need to spawn children — internal/voice,
// internal/secrets/bitwarden, internal/anthropic — are imported by
// internal/tools, so they can't import internal/tools back without an
// import cycle. internal/procx imports only stdlib + internal/log, so
// every spawn site can reach it.
package procx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

)

// SecurityGroupName is the OS group whose presence in a process's
// supplementary group set grants read access to foci's secrets file.
//
// Duplicated as a string literal here (rather than imported from
// internal/secrets) so this package stays leaf and can be imported by
// internal/secrets/bitwarden et al. without an import cycle.
const SecurityGroupName = "foci-secrets"

// securityDropGroups is the full set of supplementary groups that Setup strips
// from child processes. The primary one is foci-secrets (secrets.toml access);
// foci-askgw grants access to the askgw socket, so agent subprocesses must not
// inherit it either. Kept here (not imported) for the same leaf-package reason.
var securityDropGroups = []string{SecurityGroupName, "foci-askgw"}

// childCredential is set by Setup to drop the foci-secrets supplementary
// group from exec'd child processes while preserving all other groups
// (docker, git, etc.). If the foci-secrets group doesn't exist or
// CAP_SETGID is unavailable, this remains nil and child processes inherit
// the parent's groups.
var childCredential *syscall.Credential

// setupOnce guards Setup so a redundant call is a no-op rather than
// re-running the probe and re-emitting WARNs/debug logs.
var setupOnce sync.Once

// Setup probes whether the process has CAP_SETGID and, if so, stashes a
// Credential that drops the foci-secrets supplementary group from child
// processes spawned via Spawn / SpawnSetsid. Idempotent — safe to call
// more than once.
//
// Only foci-gw needs to call this. The foci CLI binary is a thin HTTP
// client that doesn't exec children needing the credential drop, so
// calling Setup there would just fail the probe (cron-spawned foci CLI
// lacks CAP_SETGID because cron isn't a descendant of foci-gw) and emit
// noise. If a future binary execs children that need foci-secrets
// dropped, it must call Setup explicitly during startup.
func Setup() error {
	setupOnce.Do(setupImpl)
	return setupErr
}

// setupErr records a fail-closed condition from setupImpl: the process holds the
// foci-secrets group but the child-credential drop could not be established, so
// children would inherit the group and could read secrets.toml. nil on success
// or when there is legitimately nothing to drop (root, no group, not a member).
var setupErr error

// credentialSetupError returns a non-nil error for the fail-closed condition:
// the process holds the foci-secrets group (found) but the CAP_SETGID probe
// failed (probeErr != nil), so the group cannot be dropped from children.
// Every other combination is safe. (P2-12.)
func credentialSetupError(found bool, probeErr error) error {
	if found && probeErr != nil {
		return fmt.Errorf("process holds a security group but cannot drop it from children (CAP_SETGID unavailable): %w", probeErr)
	}
	return nil
}

func setupImpl() {
	uid := os.Getuid()
	gid := os.Getgid()

	// If running as root we don't need to drop groups (and the security
	// model doesn't apply — root can read anything).
	if uid == 0 {
		return
	}

	// Resolve GIDs for all drop groups. Groups that don't exist are
	// silently skipped (nothing to protect against for that group).
	dropGIDs := make(map[uint32]string)
	for _, name := range securityDropGroups {
		grp, err := user.LookupGroup(name)
		if err != nil {
			execLog.Debugf("group %q not found — skipping", name)
			continue
		}
		g, err := strconv.ParseUint(grp.Gid, 10, 32)
		if err != nil {
			execLog.Warnf("cannot parse %s group GID %q: %v", name, grp.Gid, err)
			setupErr = fmt.Errorf("cannot parse %s group GID %q to verify drop: %w", name, grp.Gid, err)
			return
		}
		dropGIDs[uint32(g)] = name // #nosec G115
	}
	if len(dropGIDs) == 0 {
		return
	}

	// Get current supplementary groups
	currentGroups, err := syscall.Getgroups()
	if err != nil {
		execLog.Warnf("cannot read supplementary groups: %v", err)
		setupErr = fmt.Errorf("cannot read supplementary groups to verify drop: %w", err)
		return
	}

	// Build filtered list: all groups EXCEPT those in dropGIDs.
	var filteredGroups []uint32
	found := false
	for _, g := range currentGroups {
		if name, drop := dropGIDs[uint32(g)]; drop { //nolint:gosec // G115: GID values are non-negative
			found = true
			execLog.Debugf("will drop group %s (gid %d) from children", name, g)
			continue
		}
		filteredGroups = append(filteredGroups, uint32(g)) // #nosec G115
	}

	if !found {
		execLog.Debugf("process does not hold any drop groups — skipping child credential setup")
		return
	}

	// Look up primary GID
	primaryGID := uint32(gid) // #nosec G115
	if u, err := user.Current(); err == nil {
		if g, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			primaryGID = uint32(g) // #nosec G115
		}
	}

	cred := &syscall.Credential{
		Uid:    uint32(uid), // #nosec G115
		Gid:    primaryGID,
		Groups: filteredGroups,
	}

	// Probe: try spawning a trivial process with the credential.
	// If CAP_SETGID is not available, setgroups() fails and we should
	// not set the credential (which would break all exec calls).
	probe := exec.Command("true") //nolint:forbidigo // probe is the bootstrap that decides whether the credential mechanism works
	probe.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: cred,
	}
	if err := probe.Run(); err != nil {
		execLog.Warnf("cannot drop security groups (CAP_SETGID not available): %v", err)
		execLog.Warnf("child processes will inherit parent groups — add AmbientCapabilities=CAP_SETGID to systemd unit")
		setupErr = credentialSetupError(true, err)
		return
	}

	// The probe proved CAP_SETGID is in our permitted/effective sets. Now clear
	// the AMBIENT set so children don't inherit CAP_SETGID across execve and
	// re-add the dropped groups themselves (P0-1). The credential mechanism
	// keeps working because it relies on the parent's effective caps, not the
	// ambient set.
	if err := clearAmbientCaps(); err != nil {
		execLog.Warnf("could not clear ambient capabilities: %v — children may inherit CAP_SETGID", err)
	}

	childCredential = cred
	droppedNames := make([]string, 0, len(dropGIDs))
	for _, name := range dropGIDs {
		droppedNames = append(droppedNames, name)
	}
	execLog.Debugf("child credential: uid=%d gid=%d groups=%v (dropped %v)",
		uid, primaryGID, filteredGroups, droppedNames)
}

// childAttr returns a SysProcAttr that creates a new process group and
// drops the foci-secrets supplementary group. If Setup hasn't run or the
// probe failed, only Setpgid is set.
func childAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}

// childAttrSetsid is the Setsid variant of childAttr for daemonised
// children (tmux servers etc.) that need their own session.
func childAttrSetsid() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setsid: true}
	if childCredential != nil {
		attr.Credential = childCredential
	}
	return attr
}

// Spawn returns an *exec.Cmd configured with the foci-secrets group
// stripped (via childAttr) and Setpgid set so the child is in its own
// process group (allows clean process-group kill).
func Spawn(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:forbidigo // sole permitted use; see package doc
	cmd.SysProcAttr = childAttr()
	return cmd
}

// SpawnSetsid is the Setsid variant for daemonised children (tmux
// clients/servers that need their own session). Otherwise matches Spawn.
func SpawnSetsid(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:forbidigo // sole permitted use; see package doc
	cmd.SysProcAttr = childAttrSetsid()
	return cmd
}
