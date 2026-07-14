//go:build linux

package procx

import "golang.org/x/sys/unix"
import

// clearAmbientCaps empties the process ambient capability set.
//
// foci-gw is granted CAP_SETGID via systemd AmbientCapabilities (or
// setpriv --ambient-caps in Docker) so the Spawn credential mechanism can call
// setgroups() to drop the foci-secrets group from children. But ambient
// capabilities are preserved across execve(2) for non-root processes, so every
// child would otherwise inherit effective CAP_SETGID — exactly the capability
// needed to setgroups() the foci-secrets GID back on and read secrets.toml.
//
// Clearing the ambient set (and only the ambient set) closes that hole: the
// parent keeps CAP_SETGID in its permitted/effective sets, so the fork-time
// setgroups in Spawn's children (performed before execve) still works, but the
// exec'd child — non-root, no file caps, empty ambient — has its permitted and
// effective sets stripped at execve. The child ends up with no CAP_SETGID.
"foci/internal/log"

var (
	execLog = log.NewComponentLogger("exec")
)

func clearAmbientCaps() error {
	return unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
}
