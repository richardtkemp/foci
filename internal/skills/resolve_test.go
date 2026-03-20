package skills

import (
	"path/filepath"
	"testing"
)

func TestResolveDirsDefaults(t *testing.T) {
	// Verifies that with no overrides, ResolveDirs returns
	// $home/shared/skills and $workspace/skills.
	dirs := ResolveDirs("/home/foci", "/home/foci/clutch", "", "")
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d", len(dirs))
	}
	if want := filepath.Join("/home/foci", "shared", "skills"); dirs[0] != want {
		t.Errorf("shared dir = %q, want %q", dirs[0], want)
	}
	if want := filepath.Join("/home/foci/clutch", "skills"); dirs[1] != want {
		t.Errorf("agent dir = %q, want %q", dirs[1], want)
	}
}

func TestResolveDirsSharedOverride(t *testing.T) {
	// Verifies that a global config override replaces the default shared dir.
	dirs := ResolveDirs("/home/foci", "/home/foci/clutch", "/custom/shared", "")
	if dirs[0] != "/custom/shared" {
		t.Errorf("shared dir = %q, want /custom/shared", dirs[0])
	}
	if want := filepath.Join("/home/foci/clutch", "skills"); dirs[1] != want {
		t.Errorf("agent dir = %q, want %q", dirs[1], want)
	}
}

func TestResolveDirsAgentOverride(t *testing.T) {
	// Verifies that a per-agent config override replaces the default agent dir.
	dirs := ResolveDirs("/home/foci", "/home/foci/clutch", "", "/custom/agent")
	if want := filepath.Join("/home/foci", "shared", "skills"); dirs[0] != want {
		t.Errorf("shared dir = %q, want %q", dirs[0], want)
	}
	if dirs[1] != "/custom/agent" {
		t.Errorf("agent dir = %q, want /custom/agent", dirs[1])
	}
}

func TestResolveDirsBothOverrides(t *testing.T) {
	// Verifies that both overrides are applied simultaneously.
	dirs := ResolveDirs("/home/foci", "/home/foci/clutch", "/custom/shared", "/custom/agent")
	if dirs[0] != "/custom/shared" {
		t.Errorf("shared dir = %q, want /custom/shared", dirs[0])
	}
	if dirs[1] != "/custom/agent" {
		t.Errorf("agent dir = %q, want /custom/agent", dirs[1])
	}
}
