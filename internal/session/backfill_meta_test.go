package session

import "testing"

// TestBackfillSessionMetadata (#3186): every session of the agent that has
// the marker key and not the target key gets the value; other agents (one
// whose id is a prefix included), sessions without the marker and sessions
// already set are left alone, and a second run sets nothing.
func TestBackfillSessionMetadata(t *testing.T) {
	idx := tempIndex(t)
	set := func(sk, k, v string) {
		t.Helper()
		if err := idx.SetSessionMetadata(sk, k, v); err != nil {
			t.Fatal(err)
		}
	}
	set("bob/c1", MetaKeyCCResumeID, "r1")
	set("bob/c1/b17", MetaKeyCCResumeID, "r2")
	set("bob/c2", MetaKeyCCResumeID, "r3")
	set("bob/c2", MetaKeyAccount, "b")
	set("bob/c3", MetaKeyThinking, "on")
	set("bobby/c1", MetaKeyCCResumeID, "r4")

	n, err := idx.BackfillSessionMetadata("bob", MetaKeyCCResumeID, MetaKeyAccount, "a")
	if err != nil || n != 2 {
		t.Fatalf("backfill = %d, %v; want 2", n, err)
	}
	for sk, want := range map[string]string{"bob/c1": "a", "bob/c1/b17": "a", "bob/c2": "b", "bob/c3": "", "bobby/c1": ""} {
		if got, _ := idx.GetSessionMetadata(sk, MetaKeyAccount); got != want {
			t.Errorf("%s account = %q, want %q", sk, got, want)
		}
	}
	if n, err := idx.BackfillSessionMetadata("bob", MetaKeyCCResumeID, MetaKeyAccount, "a"); err != nil || n != 0 {
		t.Errorf("second run = %d, %v; want 0", n, err)
	}
}
