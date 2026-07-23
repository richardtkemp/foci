//go:build !linux

package main

import "errors"

// seal is a no-op stub on non-Linux platforms, which have no Landlock LSM.
// Always reports "unsupported" so main.go degrades gracefully rather than
// failing the build.
func seal(paths []string) (supported bool, err error) {
	return false, errors.New("Landlock is Linux-only")
}
