//go:build !linux && !darwin

package peercred

import (
	"errors"
	"net"
)

// UID is unsupported off Linux and macOS. It returns an error so the module
// still builds (and its non-peercred dependents' tests run) on other hosts. The
// real lookups live in peercred_linux.go and peercred_darwin.go.
func UID(_ *net.UnixConn) (uint32, error) {
	return 0, errors.New("peercred: peer-credential lookup is only supported on Linux and macOS")
}

// PID is unsupported on this platform.
func PID(_ *net.UnixConn) (int, error) {
	return 0, errors.New("peercred: peer-credential lookup is only supported on Linux and macOS")
}
