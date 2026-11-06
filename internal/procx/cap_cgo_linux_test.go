//go:build linux && cgo

package procx

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// A binary that links cgo cannot clear ambient capabilities on every thread, and
// clearAmbientCaps must say so rather than report success (#2007). The L2 -race
// run links cgo into its gateway (#2909) and relies on this staying fail-closed
// for any gateway that holds a security group. This file builds only with cgo,
// so a plain run skips it: `make test-one PKG=./internal/procx GOFLAGS=-race`
// runs it.
func TestClearAmbientCaps_FailsClosedWhenCgoIsLinked(t *testing.T) {
	err := clearAmbientCaps()
	if !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("clearAmbientCaps in a cgo binary = %v, want ENOTSUP", err)
	}
	if !strings.Contains(err.Error(), "build with CGO_ENABLED=0") {
		t.Errorf("the error does not name the remedy: %v", err)
	}
}
