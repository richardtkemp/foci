// Package peercred extracts the credentials of the process on the other end of
// a Unix domain socket connection (SO_PEERCRED on Linux, LOCAL_PEERCRED on macOS).
//
// It backs the same-user authentication on the gateway control socket and the
// exec bridge: even if a socket's filesystem permissions were somehow loosened,
// a connection from a different user is rejected. This is defence in depth
// alongside the 0600 socket mode the listeners set.
//
// UID is platform-specific: the real lookups live in peercred_linux.go and
// peercred_darwin.go; peercred_other.go is an erroring stub so the module still
// builds — and its non-peercred dependents' tests still run — on non-Linux hosts.
package peercred

import (
	"net"
	"os"
)

// Matches reports whether the peer's UID equals want.
func Matches(conn *net.UnixConn, want uint32) (bool, error) {
	uid, err := UID(conn)
	if err != nil {
		return false, err
	}
	return uid == want, nil
}

// MatchesSelf reports whether the peer belongs to the current process's user.
func MatchesSelf(conn *net.UnixConn) (bool, error) {
	return Matches(conn, uint32(os.Getuid())) //nolint:gosec // UID fits in uint32 on all supported platforms
}
