//go:build !linux

package ccstream

import (
	"errors"
	"os"
)

// createMemfdSystemPrompt has no non-Linux implementation: memfd_create(2)
// is a Linux-only syscall. RunBatch treats any error from this function
// (including this one) as "fall back to the temp-file path", so a non-Linux
// build degrades to the pre-memfd behaviour rather than failing the batch.
func createMemfdSystemPrompt(_ string) (*os.File, error) {
	return nil, errors.New("memfd_create not supported on this platform")
}
