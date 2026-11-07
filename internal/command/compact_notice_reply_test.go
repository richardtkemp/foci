package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// failingCompactBackend accepts /compact and ends the wait with waitErr.
type failingCompactBackend struct {
	mockPassBackend
	waitErr error
}

func (b *failingCompactBackend) WaitForTurn(context.Context) error { return b.waitErr }

// runCompactCommand runs /compact from source against a delegated agent whose
// compaction wait ends with waitErr, in a session the app (or telegram) owns.
func runCompactCommand(t *testing.T, source, platformName string, waitErr error) (Response, error) {
	t.Helper()
	const sk = "alpha/c2947"
	be := &failingCompactBackend{waitErr: waitErr}
	dm := &agent.DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return be, nil }}
	if _, err := dm.Get(context.Background(), sk); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: platformName}}}
	cc := CommandContext{
		Agent: &agent.Agent{
			DelegatedManager: dm,
			Bootstrap:        workspace.NewBootstrap(t.TempDir(), []string{}),
		},
		AgentConfig:  config.AgentConfig{ID: "alpha"},
		ConnMgr:      &compactFakeConnMgr{conn: compactFakeConn{name: platformName}},
		ResolvedLive: config.NewLiveValue(config.Resolve(cfg, config.AgentConfig{ID: "alpha"})),
	}
	ctx := tools.WithSessionKey(context.Background(), sk)
	return CompactCommand().Execute(ctx, Request{Source: source}, cc)
}

// TestCompactCommand_NoticeToldOutcomeHasNoReply is #2947's extension of
// #2891: when a failed or declined /compact has already edited the ⏳ notice
// (to ❌ or "nothing to compact") in the app chat, the command sends no
// separate reply. Elsewhere the reply stays: it is the only sign there.
func TestCompactCommand_NoticeToldOutcomeHasNoReply(t *testing.T) {
	failed := errors.New("codex: process exited")
	cases := []struct {
		name      string
		source    string
		platform  string
		waitErr   error
		wantReply string // "" = no reply and no error
		wantErr   string
	}{
		{name: "app, failed", source: "app", platform: "app", waitErr: failed},
		{name: "app, declined", source: "app", platform: "app", waitErr: delegator.ErrCompactionNoBoundary},
		{name: "telegram, failed", source: SourceTelegram, platform: "telegram", waitErr: failed, wantErr: "process exited"},
		{name: "telegram, declined", source: SourceTelegram, platform: "telegram", waitErr: delegator.ErrCompactionNoBoundary, wantReply: "Nothing to compact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := runCompactCommand(t, tc.source, tc.platform, tc.waitErr)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantReply == "" && resp.Text != "":
				t.Errorf("reply = %q, want none (the notice already told it)", resp.Text)
			case tc.wantReply != "" && !strings.Contains(resp.Text, tc.wantReply):
				t.Errorf("reply = %q, want one containing %q", resp.Text, tc.wantReply)
			}
		})
	}
}

// TestCompactCommand_ErrorBeforeNoticeKeepsReply: an error before any ⏳ was
// shown (the backend cannot be reached) is not told by a notice, so the app
// still gets the error.
func TestCompactCommand_ErrorBeforeNoticeKeepsReply(t *testing.T) {
	dm := &agent.DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return nil, errors.New("boom") }}
	cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: "app"}}}
	cc := CommandContext{
		Agent:        &agent.Agent{DelegatedManager: dm, Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{})},
		AgentConfig:  config.AgentConfig{ID: "alpha"},
		ConnMgr:      &compactFakeConnMgr{conn: compactFakeConn{name: "app"}},
		ResolvedLive: config.NewLiveValue(config.Resolve(cfg, config.AgentConfig{ID: "alpha"})),
	}
	ctx := tools.WithSessionKey(context.Background(), "alpha/c2947b")
	if _, err := CompactCommand().Execute(ctx, Request{Source: "app"}, cc); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the backend error", err)
	}
}
