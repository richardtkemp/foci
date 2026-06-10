//go:build !linux

package procx

// clearAmbientCaps is a no-op on non-Linux platforms, which have no Linux
// ambient capability set. The foci-secrets group-drop security model is
// Linux-only (see cap_linux.go).
func clearAmbientCaps() error { return nil }
