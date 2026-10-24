//go:build darwin

package peercred

import (
	"net"

	"golang.org/x/sys/unix"
)

// UID returns the UID of the process on the other end of conn. macOS has no
// SO_PEERCRED; its equivalent is the LOCAL_PEERCRED option at level SOL_LOCAL,
// which returns an xucred holding the peer's effective UID at connect time.
func UID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}
