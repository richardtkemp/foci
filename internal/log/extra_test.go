package log

import (
	"bytes"
	"strings"
	"testing"
)

// Tests for per-package "extra" verbose logging (xtra:<pkg>), gated by
// EnableExtra and emitted by Extra. Uses a unique component name so the
// process-global enabled set doesn't collide with other tests.

func TestExtraEnabled_DefaultOffAndPrefixMatch(t *testing.T) {
	const pkg = "extratest_pkg_a"

	if ExtraEnabled(pkg) {
		t.Fatalf("expected %q disabled by default", pkg)
	}

	EnableExtra(pkg)

	if !ExtraEnabled(pkg) {
		t.Fatalf("expected %q enabled after EnableExtra", pkg)
	}
	// A labelled component matches its base-package enable.
	if !ExtraEnabled(pkg + ":someagent") {
		t.Fatalf("expected labelled %q:someagent to match base enable", pkg)
	}
	// An unrelated package stays off.
	if ExtraEnabled("extratest_pkg_unrelated") {
		t.Fatal("unrelated package must stay disabled")
	}
	// A package whose name is a prefix-without-colon must NOT match.
	if ExtraEnabled(pkg + "x") {
		t.Fatalf("%q must not match enable for %q (no colon boundary)", pkg+"x", pkg)
	}
}

func TestExtra_EmitsWhenEnabledTaggedXtra(t *testing.T) {
	const pkg = "extratest_pkg_b"

	// Redirect output to a buffer; restore on exit.
	std.mu.Lock()
	prevOut := std.eventOut
	prevLevel := Level(std.level.Load())
	var buf bytes.Buffer
	std.eventOut = &buf
	std.level.Store(int32(INFO))
	std.mu.Unlock()
	defer func() {
		std.mu.Lock()
		std.eventOut = prevOut
		std.level.Store(int32(prevLevel))
		std.mu.Unlock()
	}()

	// Disabled: no output.
	Extra(pkg, "should not appear %d", 1)
	if buf.Len() != 0 {
		t.Fatalf("expected no output while disabled, got %q", buf.String())
	}

	// Enabled: tagged line at INFO.
	EnableExtra(pkg)
	Extra(pkg, "hello %d", 42)

	out := buf.String()
	if !strings.Contains(out, "[xtra:"+pkg+"]") {
		t.Fatalf("expected [xtra:%s] tag, got %q", pkg, out)
	}
	if !strings.Contains(out, "hello 42") {
		t.Fatalf("expected formatted message, got %q", out)
	}
	if !strings.Contains(out, "INFO") {
		t.Fatalf("expected INFO level, got %q", out)
	}
}

// ComponentLogger.Extra routes through the base-package enable too, even when
// the logger carries a ":label" suffix.
func TestComponentLoggerExtra_LabelMatchesBaseEnable(t *testing.T) {
	const base = "extratest_pkg_c"

	std.mu.Lock()
	prevOut := std.eventOut
	prevLevel := Level(std.level.Load())
	var buf bytes.Buffer
	std.eventOut = &buf
	std.level.Store(int32(INFO))
	std.mu.Unlock()
	defer func() {
		std.mu.Lock()
		std.eventOut = prevOut
		std.level.Store(int32(prevLevel))
		std.mu.Unlock()
	}()

	cl := NewComponentLogger(base + ":clutch")
	cl.Extra("nope")
	if buf.Len() != 0 {
		t.Fatalf("expected no output before enable, got %q", buf.String())
	}

	EnableExtra(base)
	cl.Extra("yes %s", "please")
	out := buf.String()
	if !strings.Contains(out, "[xtra:"+base+":clutch]") {
		t.Fatalf("expected [xtra:%s:clutch] tag, got %q", base, out)
	}
	if !strings.Contains(out, "yes please") {
		t.Fatalf("expected formatted message, got %q", out)
	}
}
