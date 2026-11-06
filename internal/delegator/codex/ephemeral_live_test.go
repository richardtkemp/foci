package codex

import (
	"context"
	"foci/internal/delegator"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEphemeralForkLive exercises installed Codex in an isolated home without any model calls.
// Opt in with FOCI_CODEX_BIN. It proves a pathless fork survives parent close and
// attaches without a storage-based resume; normal CI does not need Codex installed.
func TestEphemeralForkLive(t *testing.T) {
	bin := os.Getenv("FOCI_CODEX_BIN")
	if bin == "" {
		t.Skip("set FOCI_CODEX_BIN for isolated, model-free live fork probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	home := filepath.Join(t.TempDir(), "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	opts := delegator.StartOptions{AgentID: t.Name(), SessionKey: "test/c1", WorkDir: t.TempDir(), SystemPrompt: "Isolated test. No model turns.", Env: map[string]string{"CODEX_HOME": home}}
	raw, _ := newFromConfig(map[string]any{"binary": bin})
	parent := raw.(*Backend)
	if err := parent.Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	// Seed storage with an injected item, not an inference request.
	if _, err := parent.sendAndWait("thread/inject_items", threadInjectItemsParams{ThreadID: parent.SessionID(), Items: []any{map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "isolated model-free fixture"}}}}}); err != nil {
		t.Fatal(err)
	}
	fork, err := parent.ForkSession(ctx, delegator.ForkRequest{ParentSessionID: parent.SessionID(), Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	// The pending child must keep the pooled server alive even without its parent facade.
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ = newFromConfig(map[string]any{"binary": bin})
	child := raw.(*Backend)
	opts.SessionKey = "test/c1/b1"
	opts.Ephemeral = true
	opts.ResumeSessionID = fork.SessionID
	if err := child.Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	if child.SessionID() != fork.SessionID {
		t.Fatal("fork identity replaced")
	}
	if child.SessionFilePath() != "" {
		t.Fatalf("ephemeral fork has storage path %s", child.SessionFilePath())
	}
	t.Logf("installed Codex: live ephemeral fork %s adopted after parent close, no storage path", fork.SessionID)
}
