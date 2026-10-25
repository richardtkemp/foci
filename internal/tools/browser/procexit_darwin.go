//go:build darwin

package browser

import (
	"errors"

	"golang.org/x/sys/unix"
)

// watchExit returns a channel that closes when process pid exits. It holds a
// kqueue with an EVFILT_PROC/NOTE_EXIT filter, macOS's counterpart of the
// Linux pidfd: the filter binds to the process when it is registered, so a
// later reuse of the pid cannot fire it. Chromium is not our child (go-rod's
// leakless guard is its parent), so wait(2) cannot see its exit.
func watchExit(pid int) (<-chan struct{}, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	// Registering on a pid that no longer exists fails with ESRCH, as
	// pidfd_open does on Linux.
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer func() { _ = unix.Close(kq) }()
		out := make([]unix.Kevent_t, 1)
		for {
			if _, err := unix.Kevent(kq, nil, out, nil); !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	return exited, nil
}
