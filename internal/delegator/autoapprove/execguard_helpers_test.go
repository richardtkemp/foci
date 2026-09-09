package autoapprove

import (
	"os"

	"golang.org/x/sys/unix"
)

// Live filesystem predicates for the one test that needs real access(2)
// semantics rather than an injected map.
func processCanWriteForTest(path string) bool { return unix.Access(path, unix.W_OK) == nil }

func processCanExecuteForTest(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
}
