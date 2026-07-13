//go:build linux

package peercred

import (
	"net"
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
