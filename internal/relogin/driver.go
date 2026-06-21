package relogin

import (
	"context"
	"strings"
	"time"

	"foci/internal/log"
)

const logComponent = "relogin"

// maxScreenDump caps how much of a captured TUI screen we write to the log on a
// failure path. Enough to diagnose which state the login screen was actually in
// without flooding the log with the full scrollback.
const maxScreenDump = 4000

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

// comp returns the per-run log component, namespaced by agent so a re-login's
// log lines are filterable (`relogin/clutch`).
func (c *Config) comp() string {
	if c.AgentID == "" {
		return logComponent
	}
	return logComponent + "/" + c.AgentID
}

// Run executes the interactive re-login flow (Dick's 12 steps). It always
// releases the gate and kills the login pane before returning, so a failed or
// timed-out login can never leave delegated agents permanently paused. Intended
// to run in its own goroutine.
//
// The flow is inherently flake-prone — it drives a real TUI by screen-scraping,
// and its anchors come from Claude Code's login UI which we don't control — so
// every unexpected outcome is logged at WARN/ERROR, and anchor timeouts dump the
// captured screen so a future session can see exactly what state the TUI was in.
func Run(ctx context.Context, c Config) {
	c.defaults()
	comp := c.comp()
	g := c.Gate
	// Backstop: resume message processing no matter how we exit (#843). This is
	// the load-bearing safety net — every failure path below relies on it.
	defer g.Release()

	log.Infof(comp, "re-login starting (workdir=%q binary=%q anchorTimeout=%s codeTimeout=%s)",
		c.WorkDir, c.ClaudeBin, c.AnchorTimeout, c.CodeTimeout)

	notify := func(text string) {
		if c.SendMessage == nil {
			log.Warnf(comp, "no SendMessage configured — user will not see: %q", firstLineOf(text))
			return
		}
		if err := c.SendMessage(text); err != nil {
			log.Errorf(comp, "send message to user failed (%q): %v", firstLineOf(text), err)
		}
	}
	fail := func(reason string) {
		log.Errorf(comp, "re-login ABORTED: %s", reason)
		notify("🔐 Claude Code re-login failed: " + reason + "\nSend /login here to try again.")
	}
	// dumpScreen writes a captured TUI screen to the log on a failure path so the
	// reason an anchor never appeared is diagnosable after the fact.
	dumpScreen := func(label, screen string) {
		s := strings.TrimSpace(screen)
		if s == "" {
			log.Warnf(comp, "%s: captured screen was EMPTY (tmux pane gone, or claude never rendered?)", label)
			return
		}
		if len(s) > maxScreenDump {
			s = s[len(s)-maxScreenDump:] // keep the most recent (bottom) of the scrollback
		}
		log.Warnf(comp, "%s — last captured screen follows:\n%s", label, s)
	}

	pn := c.newPane(&c)

	// Step 1: spawn a separate interactive claude in tmux.
	if err := pn.create(ctx); err != nil {
		fail("could not start login session: " + err.Error())
		return
	}
	defer func() {
		if err := pn.kill(context.Background()); err != nil {
			log.Warnf(comp, "kill login pane failed (may leak a tmux session): %v", err)
		}
	}()

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
		dumpScreen("login prompt anchor never appeared", screen)
		fail("login prompt did not appear in time")
		return
	}
	url := extractLoginURL(screen)
	if url == "" {
		dumpScreen("paste anchor present but no URL extracted", screen)
		fail("could not read the login URL from the screen")
		return
	}
	log.Infof(comp, "login URL extracted (%d chars); opening capture window", len(url))

	// Step 7: open the capture window and ask the user to sign in. A failure to
	// relay the URL is fatal — the user can never complete login without it.
	g.OpenCapture(c.AgentID)
	if c.SendMessage != nil {
		if err := c.SendMessage("🔐 Sign in to re-authenticate Claude Code, then paste the code back to me:\n\n" + url); err != nil {
			fail("could not send the login URL to you: " + err.Error())
			return
		}
	} else {
		log.Errorf(comp, "no SendMessage configured — cannot relay login URL; aborting")
		return
	}

	code, ok := g.AwaitCode(c.CodeTimeout)
	if !ok {
		fail("timed out waiting for the login code")
		return
	}
	log.Infof(comp, "login code received (%d chars); submitting", len(code))

	if err := pn.sendLine(ctx, code); err != nil {
		fail("could not enter the login code: " + err.Error())
		return
	}

	// Step 8: wait for confirmation.
	if successScreen, ok := c.waitFor(ctx, pn, anchorSuccess); !ok {
		// Redact the just-submitted one-time code in case the TUI echoed it —
		// auth codes must never reach the log.
		redacted := successScreen
		if code != "" {
			redacted = strings.ReplaceAll(successScreen, code, "[redacted-code]")
		}
		dumpScreen("success anchor never appeared after code submit", redacted)
		fail("login did not complete (no confirmation on screen)")
		return
	}

	// Steps 9–11: pane killed by defer, gate released by defer.
	notify("✅ Login completed.")
	log.Infof(comp, "re-login completed for agent %s", c.AgentID)
}

// waitFor polls the pane until anchor appears in the captured screen or
// AnchorTimeout elapses. Returns the last captured screen and whether the
// anchor was found.
func (c *Config) waitFor(ctx context.Context, pn pane, anchor string) (string, bool) {
	comp := c.comp()
	deadline := time.Now().Add(c.AnchorTimeout)
	start := time.Now()
	var last string
	captureErrs := 0
	for {
		screen, err := pn.capture(ctx)
		if err != nil {
			captureErrs++
			log.Warnf(comp, "capture failed while waiting for %q (err #%d): %v", anchor, captureErrs, err)
		} else {
			last = screen
			if strings.Contains(screen, anchor) {
				return screen, true
			}
		}
		if time.Now().After(deadline) {
			log.Warnf(comp, "gave up waiting for %q after %s (%d capture errors)", anchor, time.Since(start).Round(time.Second), captureErrs)
			return last, false
		}
		c.sleep(c.PollInterval)
	}
}

// firstLineOf returns the first line of s, for compact single-line logging.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
