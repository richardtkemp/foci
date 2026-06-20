package relogin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/procx"
)

// pane is the minimal tmux surface the login driver needs. The production
// implementation (tmuxPane) shells out to tmux; tests inject a scripted fake.
//
// Deliberately a small standalone helper rather than a dependency on the
// cctmux backend's richer pane type: the login flow must not perturb (or be
// perturbed by) the working delegated backend. Some plumbing is duplicated;
// unifying into a shared tmuxpane package is a follow-up if a third caller
// appears.
type pane interface {
	create(ctx context.Context) error
	sendLine(ctx context.Context, text string) error
	enter(ctx context.Context) error
	capture(ctx context.Context) (string, error)
	kill(ctx context.Context) error
}

// tmuxPane runs an interactive `claude` in a dedicated tmux session (regular
// TUI mode — no stream-json), drivable via send-keys and readable via
// capture-pane.
type tmuxPane struct {
	session string   // tmux session name
	workDir string   // working directory for the pane
	binary  string   // claude binary (empty → "claude")
	env     []string // KEY=VALUE pairs exported before claude starts

	// exec is a test seam; nil → execTmux (a real tmux subprocess).
	exec func(ctx context.Context, args ...string) (string, error)
}

func (p *tmuxPane) run(ctx context.Context, args ...string) (string, error) {
	if p.exec != nil {
		return p.exec(ctx, args...)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := procx.SpawnSetsid(cctx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *tmuxPane) create(ctx context.Context) error {
	bin := p.binary
	if bin == "" {
		bin = "claude"
	}
	var envPrefix string
	for _, kv := range p.env {
		envPrefix += "export " + shellQuote(kv) + "; "
	}
	// Login shell so the user's PATH (and thus `claude`) resolves.
	shellCmd := "sh -l -c " + shellQuote(envPrefix+bin)
	// A wide pane keeps the login URL on as few wrapped lines as possible.
	args := []string{"new-session", "-d", "-s", p.session, "-x", "220", "-y", "50"}
	if p.workDir != "" {
		args = append(args, "-c", p.workDir)
	}
	args = append(args, shellCmd)
	out, err := p.run(ctx, args...)
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %s: %w", p.session, strings.TrimSpace(out), err)
	}
	return nil
}

// sendLine types text into the pane followed by Enter. Short literal sends suit
// the TUI prompt; the login code and "/login" are both short single-line input.
func (p *tmuxPane) sendLine(ctx context.Context, text string) error {
	if text != "" {
		if _, err := p.run(ctx, "send-keys", "-t", p.session, "-l", text); err != nil {
			return fmt.Errorf("send-keys literal: %w", err)
		}
	}
	if _, err := p.run(ctx, "send-keys", "-t", p.session, "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

func (p *tmuxPane) enter(ctx context.Context) error {
	_, err := p.run(ctx, "send-keys", "-t", p.session, "Enter")
	return err
}

func (p *tmuxPane) capture(ctx context.Context) (string, error) {
	out, err := p.run(ctx, "capture-pane", "-t", p.session, "-p", "-S", "-500")
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return out, nil
}

func (p *tmuxPane) kill(ctx context.Context) error {
	_, err := p.run(ctx, "kill-session", "-t", p.session)
	return err
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes, so
// it survives `sh -c` unmodified.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
