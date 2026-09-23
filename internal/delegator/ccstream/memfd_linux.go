//go:build linux

package ccstream

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// createMemfdSystemPrompt writes prompt into a Linux memfd (an anonymous,
// in-memory "file" with no filesystem entry — man memfd_create(2)) and
// returns it seeked back to offset 0, ready for a child to read from
// scratch. Never touches disk: not a tmpfs mount, not /tmp, nothing a crash
// leaves behind to clean up.
//
// Returned *os.File owns the fd; the caller must Close it once the child
// that inherited it (via cmd.ExtraFiles) has exited — closing earlier is
// safe for the memfd's content (the child holds its own reference from
// inheriting the fd across fork/exec) but must not happen before Start.
func createMemfdSystemPrompt(prompt string) (*os.File, error) {
	fd, err := unix.MemfdCreate("foci-system-prompt", 0)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	f := os.NewFile(uintptr(fd), "foci-system-prompt")
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write memfd: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seek memfd: %w", err)
	}
	return f, nil
}
