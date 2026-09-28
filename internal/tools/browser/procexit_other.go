//go:build !linux

package browser

import "errors"

// watchExit has no pidfd to wait on outside Linux. Stop then removes the
// profile dir without waiting for chromium to exit (the pre-#1518 behaviour).
func watchExit(int) (<-chan struct{}, error) {
	return nil, errors.New("waiting for browser exit is supported on Linux only")
}
