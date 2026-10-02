package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/session"
)

type aliasChange struct {
	agentID, platform string
	chatID            int64
}

func runSetAlias(t *testing.T, tool *Tool, sessionKey, alias string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"alias": alias})
	res, err := tool.Execute(WithSessionKey(context.Background(), sessionKey), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

// TestSetSessionAlias_NotifiesOnChange is the #2157 regression: the tool
// persisted the alias but told no one, so the app showed the old name until it
// reconnected. A successful set must fire onChanged with the chat's identity;
// a skipped or failed set must not.
func TestSetSessionAlias_NotifiesOnChange(t *testing.T) {
	idx, err := session.NewSessionIndex(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Register chat 42 on the app platform (PlatformForChat needs a row).
	if err := idx.SetChatMetadata("clutch", "app", 42, "registered", "1"); err != nil {
		t.Fatal(err)
	}
	var got []aliasChange
	tool := NewSetSessionAliasTool(idx, func(agentID, platform string, chatID int64) {
		got = append(got, aliasChange{agentID, platform, chatID})
	})

	out := runSetAlias(t, tool, "clutch/c42", "OCN: committee software")
	if !strings.HasPrefix(out, "Set conversation name") {
		t.Fatalf("set result = %q", out)
	}
	if len(got) != 1 || got[0] != (aliasChange{"clutch", "app", 42}) {
		t.Fatalf("onChanged calls = %+v, want one for clutch/app/42", got)
	}

	// A user-set (manual) name is never overwritten, so nothing changed.
	if err := idx.SetChatMetadata("clutch", "app", 42, "alias_auto", ""); err != nil {
		t.Fatal(err)
	}
	if out := runSetAlias(t, tool, "clutch/c42", "Other"); !strings.HasPrefix(out, "Skipped") {
		t.Fatalf("manual-name result = %q, want Skipped", out)
	}
	if len(got) != 1 {
		t.Fatalf("onChanged fired on a skipped set: %+v", got)
	}
}
