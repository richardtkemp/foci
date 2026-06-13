package discord

import (
	"errors"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/tooldetail"

	"github.com/bwmarrin/discordgo"
)

// TestNewBotConstruction verifies NewBot wires the allowed-user set, message
// queue, chatmeta resolver, and uses the discordgo session for both gateway and
// message I/O.
func TestNewBotConstruction(t *testing.T) {
	dg, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBot(dg, []string{"111", "222"}, nil, command.NewRegistry(), nil, "myagent")

	if !b.allowedUsers["111"] || !b.allowedUsers["222"] || b.allowedUsers["333"] {
		t.Errorf("allowed users not wired: %v", b.allowedUsers)
	}
	if b.mq == nil {
		t.Error("expected message queue")
	}
	if b.chatmeta == nil || b.agentID != "myagent" {
		t.Error("expected chatmeta resolver and agent ID")
	}
	if b.api != messageSession(dg) {
		t.Error("expected api seam to default to the discordgo session")
	}
}

// TestResolveDisplayDefaults verifies resolveDisplay snapshots the bot defaults
// when no override function is set.
func TestResolveDisplayDefaults(t *testing.T) {
	b := &Bot{display: BotDisplayConfig{
		ShowToolCalls: "preview",
		ShowThinking:  "compact",
		StreamOutput:  true,
		DisplayWidth:  44,
	}}

	d := b.resolveDisplay("any/key")
	if d.ShowToolCalls != "preview" || d.ShowThinking != "compact" || !d.StreamOutput || d.DisplayWidth != 44 {
		t.Errorf("unexpected display %+v", d)
	}
	if d.RenderOpts.MaxWidth != 44 {
		t.Errorf("expected RenderOpts.MaxWidth 44, got %d", d.RenderOpts.MaxWidth)
	}
}

// TestResolveDisplayOverrides verifies per-session overrides replace bot
// defaults field-by-field, with empty/zero values meaning "keep default".
func TestResolveDisplayOverrides(t *testing.T) {
	base := BotDisplayConfig{
		ShowToolCalls: "off",
		ShowThinking:  "off",
		StreamOutput:  true,
		DisplayWidth:  60,
	}
	tests := []struct {
		name string
		ov   DisplayOverrides
		want struct {
			tc, th string
			so     bool
			dw     int
		}
	}{
		{
			name: "no overrides keeps defaults",
			ov:   DisplayOverrides{},
			want: struct {
				tc, th string
				so     bool
				dw     int
			}{"off", "off", true, 60},
		},
		{
			name: "all fields overridden",
			ov:   DisplayOverrides{ShowToolCalls: "full", ShowThinking: "compact", StreamOutput: "false", DisplayWidth: 30},
			want: struct {
				tc, th string
				so     bool
				dw     int
			}{"full", "compact", false, 30},
		},
		{
			name: "stream on override",
			ov:   DisplayOverrides{StreamOutput: "true"},
			want: struct {
				tc, th string
				so     bool
				dw     int
			}{"off", "off", true, 60},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Bot{display: base}
			b.SetDisplayOverrideFn(func(string) DisplayOverrides { return tt.ov })
			d := b.resolveDisplay("k")
			if d.ShowToolCalls != tt.want.tc || d.ShowThinking != tt.want.th ||
				d.StreamOutput != tt.want.so || d.DisplayWidth != tt.want.dw {
				t.Errorf("got %+v, want %+v", d, tt.want)
			}
		})
	}
}

// TestStreamInterval verifies the default 1200ms interval and a configured
// override.
func TestStreamInterval(t *testing.T) {
	b := &Bot{}
	if got := b.streamInterval(); got != 1200*time.Millisecond {
		t.Errorf("expected default 1200ms, got %v", got)
	}
	b.display.StreamUpdateInterval = 3 * time.Second
	if got := b.streamInterval(); got != 3*time.Second {
		t.Errorf("expected configured 3s, got %v", got)
	}
}

// TestSessionKeyForChannelID verifies routing: secondary bots use their
// override key, primary bots derive a per-channel key, agent-less bots fall
// back to SessionKey().
func TestSessionKeyForChannelID(t *testing.T) {
	// Secondary bot: override session key wins regardless of channel.
	sec, _, _ := newTestBot(t, "")
	sec.isSecondary = true
	sec.SetSessionKeyDirect("a/c9/777")
	if got := sec.SessionKeyForChannelID(42); got != "a/c9/777" {
		t.Errorf("secondary: expected override key, got %q", got)
	}

	// Primary bot with agent ID: derives per-channel key.
	prim, _, _ := newTestBot(t, "a")
	got := prim.SessionKeyForChannelID(42)
	if got == "" || got != prim.SessionKeyForChat(42) {
		t.Errorf("primary: expected per-channel key, got %q", got)
	}

	// No agent ID, not secondary: falls back to (empty) SessionKey.
	bare := &Bot{}
	if got := bare.SessionKeyForChannelID(42); got != "" {
		t.Errorf("bare: expected empty key, got %q", got)
	}
}

// TestSessionKeyPrimaryDefaultChannel verifies SessionKey() on a primary bot
// derives the key from the default channel once one is set.
func TestSessionKeyPrimaryDefaultChannel(t *testing.T) {
	b, _, idx := newTestBot(t, "a")
	if got := b.SessionKey(); got != "" {
		t.Errorf("expected empty key before default chat, got %q", got)
	}
	if err := idx.SetDefaultChat("a", platformName, 42); err != nil {
		t.Fatal(err)
	}
	got := b.SessionKey()
	if got == "" || got != b.SessionKeyForChat(42) {
		t.Errorf("expected default-channel key, got %q", got)
	}
}

// TestSanitizeError verifies bot tokens are redacted from error text and nil
// errors/sessions are handled.
func TestSanitizeError(t *testing.T) {
	dg, err := discordgo.New("Bot sekrit-token")
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{session: dg}

	if got := b.sanitizeError(nil); got != "" {
		t.Errorf("expected empty for nil error, got %q", got)
	}
	got := b.sanitizeError(errors.New("auth failed with Bot sekrit-token here"))
	if got != "auth failed with [REDACTED] here" {
		t.Errorf("expected token redacted, got %q", got)
	}

	noSession := &Bot{}
	if got := noSession.sanitizeError(errors.New("plain")); got != "plain" {
		t.Errorf("expected passthrough without session, got %q", got)
	}
}

// TestSetToolDetailStore verifies persisted tool details are restored into the
// in-memory expansion map on startup, and nil store is a no-op.
func TestSetToolDetailStore(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.SetToolDetailStore(nil) // nil store: safe no-op
	if _, ok := b.toolStore.Load("99"); ok {
		t.Fatal("nil store should restore nothing")
	}

	dbPath := t.TempDir() + "/details.db"
	store, err := tooldetail.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	store.Store(99, "compact", "full input", "result text")

	b2, _, _ := newTestBot(t, "a")
	b2.SetToolDetailStore(store)
	entry, ok := b2.toolStore.Load("99")
	if !ok {
		t.Fatal("expected entry restored from store")
	}
	if entry.CompactText != "compact" || entry.FullInput != "full input" || entry.Result != "result text" {
		t.Errorf("unexpected restored entry %+v", entry)
	}
}

// TestSetCommandContext verifies the dispatcher is created and wired to the
// bot's session-key resolution.
func TestSetCommandContext(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.SetCommandContext(command.CommandContext{})
	if b.dispatcher == nil {
		t.Fatal("expected dispatcher")
	}
}

// TestDispatchSessionKey verifies secondary bots dispatch with their override
// key while primary bots resolve per-chat keys.
func TestDispatchSessionKey(t *testing.T) {
	sec, _, _ := newTestBot(t, "")
	sec.isSecondary = true
	sec.SetSessionKeyDirect("a/c9/777")
	if got := sec.dispatchSessionKey(42); got != "a/c9/777" {
		t.Errorf("secondary: expected override, got %q", got)
	}

	prim, _, _ := newTestBot(t, "a")
	if got := prim.dispatchSessionKey(42); got != prim.SessionKeyForChat(42) {
		t.Errorf("primary: expected per-chat key, got %q", got)
	}

	// Secondary with no override falls through to per-chat resolution.
	idle, _, _ := newTestBot(t, "a")
	idle.isSecondary = true
	if got := idle.dispatchSessionKey(42); got != idle.SessionKeyForChat(42) {
		t.Errorf("idle secondary: expected per-chat key, got %q", got)
	}
}

// TestSetSessionIndexWiresChatmeta verifies SetSessionIndex propagates the
// index into the chatmeta resolver.
func TestSetSessionIndexWiresChatmeta(t *testing.T) {
	idx := &mockIndex{}
	b := &Bot{chatmeta: nil}
	b.SetSessionIndex(idx) // nil chatmeta must not panic
	r, _ := discordTestResolver(t, "a")
	r.Index = nil
	b.chatmeta = r
	b.SetSessionIndex(idx)
	if b.chatmeta.Index == nil {
		t.Error("expected chatmeta index wired")
	}
}

// TestSetHandlerAndCommands verifies handler/registry rewiring for facet reuse.
func TestSetHandlerAndCommands(t *testing.T) {
	b := &Bot{}
	cmds := command.NewRegistry()
	b.SetHandlerAndCommands(nil, cmds)
	if b.commands != cmds {
		t.Error("expected commands registry replaced")
	}
}

// TestLoggerFallback verifies struct-literal bots fall back to the package
// logger instead of nil-dereferencing.
func TestLoggerFallback(t *testing.T) {
	b := &Bot{}
	if b.logger() != defaultLogger {
		t.Error("expected fallback to package default logger")
	}
}

// TestDisplayDefaultGetters verifies the exported display default accessors
// reflect the bot's display config (used by /display command plumbing).
func TestDisplayDefaultGetters(t *testing.T) {
	b := &Bot{display: BotDisplayConfig{
		ShowToolCalls: "full",
		ShowThinking:  "compact",
		StreamOutput:  true,
		DisplayWidth:  33,
	}}
	if b.ShowToolCallsDefault() != "full" || b.ShowThinkingDefault() != "compact" ||
		!b.StreamOutputDefault() || b.DisplayWidthDefault() != 33 {
		t.Error("display default getters do not reflect config")
	}
}

// TestSetSecondary verifies SetSecondary marks the bot and records its pool.
func TestSetSecondary(t *testing.T) {
	p := NewPool()
	b := &Bot{}
	b.SetSecondary(p)
	if !b.isSecondary || b.pool != p {
		t.Error("expected secondary flag and pool set")
	}
}
