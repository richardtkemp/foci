//go:build !unix

package procx

// PassOpenFileLimitToChildren is a no-op where there is no RLIMIT_NOFILE.
func PassOpenFileLimitToChildren() (uint64, error) { return 0, nil }
