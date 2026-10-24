//go:build !linux && !darwin

package procinfo

import "time"

// MemTotalKB is unsupported on this platform.
func MemTotalKB() (int64, error) { return 0, ErrUnsupported }

// RSSKB is unsupported on this platform.
func RSSKB(int) (int64, error) { return 0, ErrUnsupported }

// Snapshot is unsupported on this platform.
func Snapshot() ([]Proc, error) { return nil, ErrUnsupported }

// ChildrenFunc finds no children on this platform.
func ChildrenFunc() func(int) []int { return func(int) []int { return nil } }

// Cmdline is unsupported on this platform.
func Cmdline(int) ([]string, error) { return nil, ErrUnsupported }

// Uptime is unsupported on this platform.
func Uptime() (time.Duration, error) { return 0, ErrUnsupported }
