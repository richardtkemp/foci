//go:build !linux && !darwin

package browser

import "errors"

// watchExit has no pidfd or kqueue to wait on outside Linux and macOS. Stop
// then removes the profile dir without waiting for chromium to exit (the
// pre-#1518 behaviour).
func watchExit(int) (<-chan struct{}, error) {
	return nil, errors.New("waiting for browser exit is supported on Linux and macOS only")
}
