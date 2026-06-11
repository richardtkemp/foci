// Package peercred extracts the credentials of the process on the other end of
// a Unix domain socket connection via SO_PEERCRED.
//
// It backs the same-user authentication on the gateway control socket and the
// exec bridge: even if a socket's filesystem permissions were somehow loosened,
// a connection from a different user is rejected. This is defence in depth
// alongside the 0600 socket mode the listeners set.
package peercred

import (
	"net"
	"os"
	"syscall"
)

// UID returns the UID of the process on the other end of conn using SO_PEERCRED.
func UID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

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
