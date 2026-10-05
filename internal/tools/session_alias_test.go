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

// TestSetSessionAlias_ReplaceManual is #2166: the user asked the agent to
// rename a chat they had named by hand, and the manual-name guard refused with
// no way round it. replace_manual overrides the guard, and the result counts as
// a manual name (the user asked for it), so a later unprompted call cannot
// clobber it.
func TestSetSessionAlias_ReplaceManual(t *testing.T) {
	idx, err := session.NewSessionIndex(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Chat 42 on the app platform, renamed by hand to "OCN" (alias_auto unset).
	if err := idx.SetChatAliasUnique("clutch", "app", 42, "OCN"); err != nil {
		t.Fatal(err)
	}
	tool := NewSetSessionAliasTool(idx, nil)

	out := runSetAlias(t, tool, "clutch/c42", "OCN - job offer")
	if !strings.HasPrefix(out, "Skipped") || !strings.Contains(out, "--replace-manual") {
		t.Fatalf("plain call on a manual name = %q, want Skipped naming --replace-manual", out)
	}

	params, _ := json.Marshal(map[string]any{"alias": "OCN - job offer", "replace_manual": true})
	res, err := tool.Execute(WithSessionKey(context.Background(), "clutch/c42"), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Text, "Set conversation name") {
		t.Fatalf("replace_manual result = %q", res.Text)
	}
	if got, _ := idx.GetChatMetadata("clutch", "app", 42, "alias"); got != "OCN - job offer" {
		t.Fatalf("alias = %q, want the replacement", got)
	}
	if got, _ := idx.GetChatMetadata("clutch", "app", 42, "alias_auto"); got == "1" {
		t.Fatalf("alias_auto = %q after a user-requested rename, want manual", got)
	}
	if out := runSetAlias(t, tool, "clutch/c42", "Something else"); !strings.HasPrefix(out, "Skipped") {
		t.Fatalf("unprompted call after a requested rename = %q, want Skipped", out)
	}
}
