package discord

// #2043: a Discord outage at boot must not hold up foci startup. The gateway
// open runs in the background (netretry schedule) and the bot only becomes a
// live connection once it succeeds.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

// switchableGateway fails every gateway open with the boot DNS error until
// succeed() is called.
type switchableGateway struct {
	failing  atomic.Bool
	attempts atomic.Int32

	// shutdownAt, if set, is called from inside attempt cancelAt — shutdown
	// arriving mid-attempt, the moment after which no attempt may start.
	cancelAt   int32
	shutdownAt func()
}

func (g *switchableGateway) succeed() { g.failing.Store(false) }

func withSwitchableGateway(t *testing.T, err error) *switchableGateway {
	t.Helper()
	withFastGatewayBackoff(t)
	g := &switchableGateway{}
	g.failing.Store(true)
	orig := openGateway
	openGateway = func(*discordgo.Session) error {
		if n := g.attempts.Add(1); n == g.cancelAt && g.shutdownAt != nil {
			g.shutdownAt()
		}
		if g.failing.Load() {
			return err
		}
		return nil
	}
	t.Cleanup(func() { openGateway = orig })
	return g
}

// within runs fn and fails the test if it has not returned by the deadline.
// The deadline is a hang guard only, far above any real cost.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked while Discord was unreachable: foci startup would wait on it", what)
	}
}

// eventually polls cond until it holds; the deadline is a hang guard.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestBackgroundConnect_FailingGatewayDoesNotBlockStartup(t *testing.T) {
	gw := withSwitchableGateway(t, bootDNSError())
	var mgr *BotManager
	within(t, "SetupAgent", func() { mgr = setupDiscordAgent(t) })
	cm := platform.NewConnectionManagerAdapter[*Bot](mgr)
	if cm.Primary("clutch") != nil {
		t.Fatal("primary bot is a live connection before the gateway ever opened")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); mgr.Wait() }()
	within(t, "StartAll", func() { mgr.StartAll(ctx) })

	eventually(t, "3 failed gateway opens", func() bool { return gw.attempts.Load() >= 3 })
	if cm.Primary("clutch") != nil {
		t.Fatal("primary attached while the gateway was still failing")
	}

	gw.succeed()
	eventually(t, "primary to attach after the gateway opens", func() bool { return cm.Primary("clutch") != nil })
	bot := cm.Primary("clutch").(*Bot)
	fs := &fakeSession{}
	bot.api = fs
	if err := bot.SendToSession("clutch/c42", "hello after attach"); err != nil {
		t.Fatalf("send via attached bot: %v", err)
	}
	if got := fs.lastSend(t); got.channelID != "42" || !strings.Contains(got.content, "hello after attach") {
		t.Fatalf("sent %+v, want the message on channel 42", got)
	}
}

func TestBackgroundConnect_ShutdownCancelsPendingGatewayRetry(t *testing.T) {
	gw := withSwitchableGateway(t, bootDNSError())
	var mgr *BotManager
	within(t, "SetupAgent", func() { mgr = setupDiscordAgent(t) })

	ctx, cancel := context.WithCancel(context.Background())
	gw.cancelAt, gw.shutdownAt = 3, cancel
	within(t, "StartAll", func() { mgr.StartAll(ctx) })
	eventually(t, "shutdown during attempt 3", func() bool { return ctx.Err() != nil })
	within(t, "Wait after shutdown", mgr.Wait)
	// Cancelled during attempt 3: the loop must stop there. Checked again
	// after 20 retry periods, so a loop still running would show up.
	time.Sleep(20 * time.Millisecond)
	if got := gw.attempts.Load(); got != 3 {
		t.Fatalf("gateway opens = %d after shutdown during attempt 3, want 3", got)
	}
	if platform.NewConnectionManagerAdapter[*Bot](mgr).Primary("clutch") != nil {
		t.Fatal("bot attached although its connect was cancelled")
	}
}

// Close 4004 (bad token) still stops the retry at once, in the background
// as it did on the setup path.
func TestBackgroundConnect_BadTokenStopsRetry(t *testing.T) {
	gw := withSwitchableGateway(t, &websocket.CloseError{Code: 4004, Text: "Authentication failed."})
	var mgr *BotManager
	within(t, "SetupAgent", func() { mgr = setupDiscordAgent(t) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	within(t, "StartAll", func() { mgr.StartAll(ctx) })
	// No cancel: the connect goroutine must end on its own.
	within(t, "the connect goroutine to give up on close 4004", mgr.Wait)
	if got := gw.attempts.Load(); got != 1 {
		t.Errorf("gateway opens = %d, want 1 (auth failure must not retry)", got)
	}
	if platform.NewConnectionManagerAdapter[*Bot](mgr).Primary("clutch") != nil {
		t.Error("bot attached despite a rejected token")
	}
}
