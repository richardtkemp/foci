//go:build linux

package browser

import (
	"errors"

	"golang.org/x/sys/unix"
)

// watchExit returns a channel that closes when process pid exits. It holds a
// pidfd, so the watch stays bound to this process even if the pid is reused
// later. Chromium is not our child (go-rod's leakless guard is its parent), so
// wait(2) cannot see its exit; a pidfd becomes readable when it does.
func watchExit(pid int) (<-chan struct{}, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer func() { _ = unix.Close(fd) }()
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} // #nosec G115 - the kernel's fd type is a C int, so any fd fits int32
		for {
			if _, err := unix.Poll(fds, -1); !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	return exited, nil
}
