package relogin

import (
	"context"
	"strings"
	"time"

	"foci/internal/log"
)

const logComponent = "relogin"

// Config parameterises a single re-login run.
type Config struct {
	AgentID   string // the agent whose 401 triggered this; owns the capture window
	WorkDir   string // working directory for the login `claude` process
	ClaudeBin string // claude binary override (empty → "claude")
	Env       []string
	Gate      *Gate // the claimed gate (driver releases it on every exit path)

	// SendMessage pushes a message to the triggering agent's user (Telegram).
	SendMessage func(text string) error

	// Timeouts (zero → defaults).
	SettleDelay   time.Duration // step 2: wait for the TUI to settle (default 5s)
	EnterDelay    time.Duration // step 4: wait before the second Enter (default 1s)
	AnchorTimeout time.Duration // waiting for the URL prompt / success line (default 90s)
	CodeTimeout   time.Duration // waiting for the user to paste the code (default 10m)
	PollInterval  time.Duration // capture-pane poll cadence (default 1s)

	// Test seams.
	newPane func(c *Config) pane
	sleep   func(d time.Duration)
}

func (c *Config) defaults() {
	if c.SettleDelay == 0 {
		c.SettleDelay = 5 * time.Second
	}
	if c.EnterDelay == 0 {
		c.EnterDelay = 1 * time.Second
	}
	if c.AnchorTimeout == 0 {
		c.AnchorTimeout = 90 * time.Second
	}
	if c.CodeTimeout == 0 {
		c.CodeTimeout = 10 * time.Minute
	}
	if c.PollInterval == 0 {
		c.PollInterval = 1 * time.Second
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	if c.newPane == nil {
		c.newPane = func(cfg *Config) pane {
			return &tmuxPane{
				session: "foci-cc-login",
				workDir: cfg.WorkDir,
				binary:  cfg.ClaudeBin,
				env:     cfg.Env,
			}
		}
	}
}

// Run executes the interactive re-login flow (Dick's 12 steps). It always
// releases the gate and kills the login pane before returning, so a failed or
// timed-out login can never leave delegated agents permanently paused. Intended
// to run in its own goroutine.
func Run(ctx context.Context, c Config) {
	c.defaults()
	g := c.Gate
	// Backstop: resume message processing no matter how we exit (#843). This is
	// the load-bearing safety net — every failure path below relies on it.
	defer g.Release()

	notify := func(text string) {
		if c.SendMessage == nil {
			return
		}
		if err := c.SendMessage(text); err != nil {
			log.Warnf(logComponent, "send message failed: %v", err)
		}
	}
	fail := func(reason string) {
		log.Warnf(logComponent, "re-login aborted: %s", reason)
		notify("🔐 Claude Code re-login failed: " + reason + "\nRun `claude /login` on the host to recover.")
	}

	pn := c.newPane(&c)

	// Step 1: spawn a separate interactive claude in tmux.
	if err := pn.create(ctx); err != nil {
		fail("could not start login session: " + err.Error())
		return
	}
	defer func() { _ = pn.kill(context.Background()) }()

	notify("🔐 Claude Code authentication expired — starting automatic re-login…")

	// Step 2: let the TUI settle.
	c.sleep(c.SettleDelay)

	// Step 3: send /login + Enter.
	if err := pn.sendLine(ctx, "/login"); err != nil {
		fail("could not send /login: " + err.Error())
		return
	}

	// Step 4: confirm the menu selection with a second Enter.
	c.sleep(c.EnterDelay)
	if err := pn.enter(ctx); err != nil {
		fail("could not confirm login: " + err.Error())
		return
	}

	// Steps 5–6: wait for the URL prompt, extract and relay the URL.
	screen, ok := c.waitFor(ctx, pn, anchorPaste)
	if !ok {
		fail("login prompt did not appear in time")
		return
	}
	url := extractLoginURL(screen)
	if url == "" {
		fail("could not read the login URL from the screen")
		return
	}

	// Step 7: open the capture window and ask the user to sign in.
	g.OpenCapture(c.AgentID)
	notify("🔐 Sign in to re-authenticate Claude Code, then paste the code back to me:\n\n" + url)

	code, ok := g.AwaitCode(c.CodeTimeout)
	if !ok {
		fail("timed out waiting for the login code")
		return
	}

	if err := pn.sendLine(ctx, code); err != nil {
		fail("could not enter the login code: " + err.Error())
		return
	}

	// Step 8: wait for confirmation.
	if _, ok := c.waitFor(ctx, pn, anchorSuccess); !ok {
		fail("login did not complete (no confirmation on screen)")
		return
	}

	// Steps 9–11: pane killed by defer, gate released by defer.
	notify("✅ Login completed.")
	log.Infof(logComponent, "re-login completed for agent %s", c.AgentID)
}

// waitFor polls the pane until anchor appears in the captured screen or
// AnchorTimeout elapses. Returns the last captured screen and whether the
// anchor was found.
func (c *Config) waitFor(ctx context.Context, pn pane, anchor string) (string, bool) {
	deadline := time.Now().Add(c.AnchorTimeout)
	var last string
	for {
		screen, err := pn.capture(ctx)
		if err != nil {
			log.Warnf(logComponent, "capture failed while waiting for %q: %v", anchor, err)
		} else {
			last = screen
			if strings.Contains(screen, anchor) {
				return screen, true
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		c.sleep(c.PollInterval)
	}
}
