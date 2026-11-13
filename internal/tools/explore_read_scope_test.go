package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Tests for #3100: an API explore spawn's read is area-confined (#3076) only
// while tools.explore_confine_reads is on; off by default it reads any path,
// and the secrets blocklist applies either way.

func TestReadTool_AreaConfinementToggle(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	var confine atomic.Bool
	read := NewReadTool(nil, f.ws, nil, append(f.opts, WithAreaConfinement(confine.Load))...)
	outside := filepath.Join(f.outside, "data.txt")
	raw, _ := json.Marshal(map[string]string{"path": outside})

	res, err := read.Execute(context.Background(), raw)
	if err != nil || !strings.Contains(res.Text, "hello") {
		t.Fatalf("confinement off: outside read should succeed, got %v / %q", err, res.Text)
	}

	confine.Store(true) // a live config edit
	if _, err := read.Execute(context.Background(), raw); err == nil || !strings.Contains(err.Error(), FileAllowedPathsKey) {
		t.Fatalf("confinement on: outside read should be refused naming %s, got %v", FileAllowedPathsKey, err)
	}
}

func TestReadTool_AreaConfinementOffKeepsBlocklist(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	store, blocked := blockedSecretsFile(t) // a secrets.toml outside f.ws
	read := NewReadTool(store, f.ws, nil, append(f.opts, WithAreaConfinement(func() bool { return false }))...)
	raw, _ := json.Marshal(map[string]string{"path": blocked})
	if _, err := read.Execute(context.Background(), raw); err == nil {
		t.Fatal("a blocked path was read with area confinement off")
	}
}

func TestSpawnExploreToolSet_UsesExploreRead(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register(&Tool{Name: "read", Description: "main read"})
	exploreRead := &Tool{Name: "read", Description: "explore read"}
	if _, set := spawnExploreToolSet(reg, exploreRead); set["read"] != exploreRead {
		t.Errorf("explore spawn should get the explore read, got %+v", set["read"])
	}
	if _, set := spawnExploreToolSet(NewRegistry(), exploreRead); set["read"] != nil {
		t.Error("a registry without read (denied by config) must not gain one")
	}
}
