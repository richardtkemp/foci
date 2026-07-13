//go:build !linux

package peercred

import (
	"errors"
	"net"
)

// UID is unsupported off Linux, which lacks SO_PEERCRED. It returns an error so
// the module still builds (and its non-peercred dependents' tests run) on hosts
// like a Mac. The real lookup lives in peercred_linux.go.
func UID(_ *net.UnixConn) (uint32, error) {
	return 0, errors.New("peercred: SO_PEERCRED peer-credential lookup is only supported on Linux")
}
