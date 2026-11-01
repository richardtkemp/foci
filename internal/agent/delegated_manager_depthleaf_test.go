package agent

import (
	"context"
	"testing"
)

// TestGet_DepthLeafReachesStart (#2625): the manager asks DepthLeaf at each
// backend Start and passes the answer in StartOptions.DepthLeaf, so only a
// depth leaf's backend refuses subagent paths; every other session (and a
// manager with no DepthLeaf) starts as before.
func TestGet_DepthLeafReachesStart(t *testing.T) {
	t.Parallel()
	mgr, mocks := newTestManager(t, nil)
	mgr.DepthLeaf = func(sk string) bool { return sk == "test-agent/leaf" }
	for _, sk := range []string{"test-agent/leaf", "test-agent/root"} {
		if _, err := mgr.Get(context.Background(), sk); err != nil {
			t.Fatalf("Get %s: %v", sk, err)
		}
	}
	if !(*mocks)[0].startOpts.DepthLeaf {
		t.Error("leaf session started without DepthLeaf")
	}
	if (*mocks)[1].startOpts.DepthLeaf {
		t.Error("root session started with DepthLeaf")
	}

	plain, plainMocks := newTestManager(t, nil)
	if _, err := plain.Get(context.Background(), "test-agent/leaf"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if (*plainMocks)[0].startOpts.DepthLeaf {
		t.Error("a manager with no DepthLeaf started a session as a leaf")
	}
}
