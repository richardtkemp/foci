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
