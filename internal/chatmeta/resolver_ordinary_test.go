package chatmeta

// resolver_ordinary_test.go: the characterisation that an ordinary
// (Telegram/Discord-style) chat registration is unchanged by the never-default
// support. Kept in a file that compiles against the base commit (no
// new-symbol references), so the red gate can prove it passes there.

import (
	"path/filepath"
	"testing"

	"foci/internal/log"
	"foci/internal/session"
)

// ordinaryLogger / ordinaryResolver are this file's own resolver fixtures
// (named distinctly from resolver_test.go's testLogger/testResolver, so this
// characterisation file compiles on the base commit standalone).
var ordinaryLogger = log.NewComponentLogger("chatmeta-test")

func ordinaryResolver(t *testing.T) *Resolver {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{
		Index:        idx,
		AgentID:      "test-agent",
		PlatformName: "test",
		Logger:       func() *log.ComponentLogger { return ordinaryLogger },
	}
}

// TestResolverRegisterChat_OrdinaryChatWritesNoFlag pins what an ordinary
// (Telegram/Discord-style) RegisterChat persists: exactly the 'registered'
// ownership row and nothing else — no never_default flag, no target row — so
// the never-default support added for Slack (#2459) changes nothing for the
// existing transports (they keep never_default unset and stay eligible as
// default chats).
func TestResolverRegisterChat_OrdinaryChatWritesNoFlag(t *testing.T) {
	r := ordinaryResolver(t)
	r.RegisterChat(31)
	if v, err := r.Index.GetChatMetadata("test-agent", "test", 31, "registered"); err != nil || v != "true" {
		t.Errorf("registered row = %q, %v; want \"true\", nil", v, err)
	}
	for _, key := range []string{"never_default", "slack_target"} {
		if v, err := r.Index.GetChatMetadata("test-agent", "test", 31, key); err == nil && v != "" {
			t.Errorf("ordinary registration wrote %s=%q; want no row", key, v)
		}
	}
}
