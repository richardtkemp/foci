package discord

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// restrictedTestBot returns a test bot whose allowlist admits only user 111
// (allowed_users_only semantics), for press-access tests.
func restrictedTestBot(t *testing.T) (*Bot, *fakeSession) {
	t.Helper()
	b, fs, _ := newTestBot(t, "a")
	b.allowedUsers = map[string]bool{"111": true}
	b.allowedUsersOnly = true
	return b, fs
}

// guildComponentInteraction builds a button press from a guild member: the
// shape discordgo delivers for a press in a guild (Member.User set, no
// top-level User).
func guildComponentInteraction(channelID, msgID, customID, guildID, memberUserID string) *discordgo.InteractionCreate {
	i := componentInteraction(channelID, msgID, customID)
	i.User = nil
	i.GuildID = guildID
	i.Member = &discordgo.Member{User: &discordgo.User{ID: memberUserID, Username: "user-" + memberUserID}}
	return i
}

// dmComponentInteraction builds a button press from a DM user (no member).
func dmComponentInteraction(channelID, msgID, customID, userID string) *discordgo.InteractionCreate {
	i := componentInteraction(channelID, msgID, customID)
	i.User = &discordgo.User{ID: userID, Username: "user-" + userID}
	return i
}

func TestHandleComponentInteraction_RejectedMemberCommandNotDispatched(t *testing.T) {
	// Proves a cmd: press from a guild member outside the allowlist dispatches
	// no command, edits nothing, and is still acknowledged (#2276).
	b, fs := restrictedTestBot(t)
	var runs atomic.Int32
	b.commands.Register(&command.Command{
		Name: "ping",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			runs.Add(1)
			return command.Response{Text: "pong"}, nil
		},
	})
	b.SetCommandContext(commandTestContext())

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "cmd:/ping", "g1", "999"))

	if got := runs.Load(); got != 0 {
		t.Errorf("command ran %d times, want 0", got)
	}
	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_RejectedMemberInteractiveNotFired(t *testing.T) {
	// Proves an im: press from a disallowed member neither fires the prompt's
	// callback nor consumes its registration (#2276).
	b, fs := restrictedTestBot(t)
	var fired atomic.Bool
	platform.RestoreInteractiveCallback("dc-access-im", "", nil,
		[]platform.ButtonChoice{{Label: "Allow", Data: "allow"}},
		func(platform.ButtonChoice) string { fired.Store(true); return "✅ Allow" },
		nil, time.Now())

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "im:dc-access-im:0", "g1", "999"))

	if fired.Load() {
		t.Error("disallowed press fired the interactive callback")
	}
	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_DMUserRejected(t *testing.T) {
	// Proves a DM press (i.User, no member) from a user outside the allowlist
	// is rejected: no action, still acknowledged (#2276).
	b, fs := restrictedTestBot(t)
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), dmComponentInteraction("42", "100", "tc:show", "999"))

	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_NoPresserRejected(t *testing.T) {
	// Proves an interaction carrying no presser at all (no member, no user)
	// is rejected rather than acted on (#2276).
	b, fs := restrictedTestBot(t)
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})
	i := componentInteraction("42", "100", "tc:show")
	i.User = nil // strip the fixture's presser

	b.handleComponentInteraction(context.Background(), i)

	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_ForeignGuildRejected(t *testing.T) {
	// Proves a press from an allowed user in a guild other than the bot's own
	// is rejected by the same guild rule as onMessageCreate (#2276).
	b, fs, _ := newTestBot(t, "a") // open access: anyone on the allowlist rule
	b.guildID = "home"
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "foreign", "111"))

	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_AllowedMemberPressWorks(t *testing.T) {
	// Characterises the allowed path under a restrictive allowlist: a press
	// from the allowed member still expands the tool call (#2276).
	b, fs := restrictedTestBot(t)
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "g1", "111"))

	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("expected tool expansion, got %q", got.content)
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_RejectionLoggedAtWarn(t *testing.T) {
	// Proves a rejected press logs one WARN naming the presser, mirroring the
	// message-rejection log operators watch for strangers finding the bot
	// (#2276).
	var (
		mu      sync.Mutex
		entries []struct {
			level     log.Level
			component string
			msg       string
		}
	)
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, struct {
			level     log.Level
			component string
			msg       string
		}{level, component, msg})
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b, _ := restrictedTestBot(t)
	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "g1", "999"))

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, e := range entries {
		if e.level == log.WARN &&
			strings.HasPrefix(e.component, "discord") &&
			strings.HasPrefix(e.msg, "rejected interaction from ") &&
			strings.Contains(e.msg, "999 (user-999)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WARN 'rejected interaction from 999 (user-999)', got %+v", entries)
	}
}

func TestHandleComponentInteraction_RespondsDeferredMessageUpdate(t *testing.T) {
	// Proves every press — rejected or allowed — is acknowledged with a
	// deferred message update, the ack type that stops the presser's
	// spinner without editing anything itself (#2303, shipping the #2276
	// reviewer's response-type check).
	b, fs := restrictedTestBot(t)
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "g1", "999"))
	if fs.interactionResponds != 1 || fs.lastInteractionResponseType != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Errorf("after rejected press: responds=%d type=%d, want 1/%d",
			fs.interactionResponds, fs.lastInteractionResponseType, discordgo.InteractionResponseDeferredMessageUpdate)
	}
	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0 after rejected press", len(fs.edits))
	}

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "g1", "111"))
	if fs.interactionResponds != 2 || fs.lastInteractionResponseType != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Errorf("after allowed press: responds=%d type=%d, want 2/%d",
			fs.interactionResponds, fs.lastInteractionResponseType, discordgo.InteractionResponseDeferredMessageUpdate)
	}
	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("expected tool expansion after allowed press, got %q", got.content)
	}
}

func TestHandleComponentInteraction_RejectedThenAllowedIMPrompt(t *testing.T) {
	// Proves a rejected im: press neither fires the prompt's callback nor
	// consumes its registration: a later im: press from the allowed member
	// on the same registration fires it exactly once (#2303, shipping the
	// #2276 reviewer's im-after-rejection check for discord).
	b, fs := restrictedTestBot(t)
	var fired atomic.Int32
	platform.RestoreInteractiveCallback("dc-2303-im", "", nil,
		[]platform.ButtonChoice{{Label: "Allow", Data: "allow"}},
		func(platform.ButtonChoice) string { fired.Add(1); return "✅ Allow" },
		nil, time.Now())

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "im:dc-2303-im:0", "g1", "999"))
	if got := fired.Load(); got != 0 {
		t.Errorf("callback fired %d times after rejected press, want 0", got)
	}
	if fs.interactionResponds != 1 || len(fs.edits) != 0 {
		t.Errorf("after rejected press: responds=%d edits=%d, want 1/0", fs.interactionResponds, len(fs.edits))
	}

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "im:dc-2303-im:0", "g1", "111"))
	if got := fired.Load(); got != 1 {
		t.Errorf("callback fired %d times after allowed press, want 1", got)
	}
	if fs.interactionResponds != 2 || len(fs.edits) != 1 {
		t.Errorf("after allowed press: responds=%d edits=%d, want 2/1", fs.interactionResponds, len(fs.edits))
	}
}

func TestHandleComponentInteraction_MemberWithoutUserRejected(t *testing.T) {
	// Proves a press whose Member carries no User (and no top-level User)
	// has no presser to authorize and is rejected — no action, still
	// acknowledged — while Member.User nil with a top-level User 111 runs
	// (#2303, shipping the #2276 reviewer's Member-without-User check).
	b, fs := restrictedTestBot(t)
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	i := componentInteraction("42", "100", "tc:show")
	i.User = nil
	i.Member = &discordgo.Member{}
	b.handleComponentInteraction(context.Background(), i)
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0 after memberless press", len(fs.edits))
	}

	i = componentInteraction("42", "100", "tc:show")
	i.Member = &discordgo.Member{}
	b.handleComponentInteraction(context.Background(), i)
	if fs.interactionResponds != 2 {
		t.Errorf("responds = %d, want 2 (each press acknowledged)", fs.interactionResponds)
	}
	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("expected tool expansion for top-level-User press, got %q", got.content)
	}
}

func TestHandleComponentInteraction_HomeGuildAndDMPass(t *testing.T) {
	// Proves a configured guild restriction lets through presses from the
	// bot's own guild and from DMs (empty GuildID) — the same rule as
	// onMessageCreate (#2303, shipping the #2276 reviewer's G1/DM check).
	b, fs, _ := newTestBot(t, "a") // open access: the guild rule is under test
	b.guildID = "G1"
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "G1", "111"))
	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("home-guild press: expected tool expansion, got %q", got.content)
	}

	b.handleComponentInteraction(context.Background(), dmComponentInteraction("42", "100", "tc:show", "111"))
	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("DM press: expected tool expansion, got %q", got.content)
	}
	if fs.interactionResponds != 2 {
		t.Errorf("responds = %d, want 2", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_LockdownEmptyAllowlistBlocksAll(t *testing.T) {
	// Proves lockdown (empty allowlist with allowed_users_only true) blocks
	// every presser — the 111 fixture presser included — while still
	// acknowledging the press (#2303, shipping the #2276 reviewer's
	// lockdown check for discord).
	b, fs, _ := newTestBot(t, "a")
	b.allowedUsersOnly = true
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), componentInteraction("42", "100", "tc:show"))

	if len(fs.edits) != 0 {
		t.Errorf("edits = %d, want 0", len(fs.edits))
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1 (press acknowledged)", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_OpenAccessAnyMemberWorks(t *testing.T) {
	// Proves open access (empty allowlist, allowed_users_only false) lets a
	// member other than the 111 fixture presser through (#2303, shipping
	// the #2276 reviewer's open-access check for discord).
	b, fs, _ := newTestBot(t, "a")
	storeToolResult(b, "100", turn.ToolResultEntry{CompactText: "c", FullInput: "full input"})

	b.handleComponentInteraction(context.Background(), guildComponentInteraction("42", "100", "tc:show", "g1", "999"))

	if got := fs.lastEdit(t); !strings.Contains(got.content, "full input") {
		t.Errorf("expected tool expansion, got %q", got.content)
	}
	if fs.interactionResponds != 1 {
		t.Errorf("responds = %d, want 1", fs.interactionResponds)
	}
}

func TestHandleComponentInteraction_ThinkingUsesTurnSessionKey(t *testing.T) {
	// Proves the thinking toggle resolves display overrides with the
	// pressing channel's session key — the key the turn that rendered the
	// message used — so no toggle ever registers chat 0 (#2303).
	var (
		ovMu  sync.Mutex
		ovKey string
	)
	b, fs, idx := newTestBot(t, "a")
	b.displayOverrideFn = func(sessionKey string) DisplayOverrides {
		ovMu.Lock()
		defer ovMu.Unlock()
		ovKey = sessionKey
		return DisplayOverrides{}
	}
	b.thinkingStore.Store(int64(100), thinkingEntry{responseText: "the answer", thinkingText: "deep thought"})

	b.handleComponentInteraction(context.Background(), componentInteraction("42", "100", "th:show"))

	ovMu.Lock()
	defer ovMu.Unlock()
	if want := b.SessionKeyForChannelID(42); ovKey != want {
		t.Errorf("display overrides resolved for %q, want %q", ovKey, want)
	}
	if got, _ := idx.GetChatMetadata("a", "discord", 0, "registered"); got != "" {
		t.Errorf("chat 0 registered metadata = %q, want empty (no chat-0 registration)", got)
	}
	if got := fs.lastEdit(t); !strings.Contains(got.content, "deep thought") {
		t.Errorf("expected thinking expansion, got %q", got.content)
	}
}
