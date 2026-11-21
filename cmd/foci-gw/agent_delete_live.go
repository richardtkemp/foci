package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"foci/internal/agentrename"
	"foci/internal/app"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/configapprove"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
)

// agent_delete_live.go: the live delete of one agent (#3210). Like the live
// rename it runs inside the gateway, so only the deleted agent stops: it is
// stopped as a shutdown stops it (stopLive), then everything foci keeps for
// it is removed on disk (agentrename.Delete), and it is not started again.
//
// Nothing changes before TWO confirmations (Dick, 2026-11-21), each naming
// the agent and what goes. In the app both are the app-wide dialog, the
// second with the big red warning banner ("Big and red on second one");
// on Telegram/Discord both are button messages in the chat the command came
// from. Cancelling or letting either expire changes nothing.

// deleteConfirmTimeout is how long each confirmation waits (the
// config-change default, #479).
var deleteConfirmTimeout = configapprove.DefaultTimeout

// deletePrompt is one confirmation.
type deletePrompt struct {
	ID      string
	Header  string
	Text    string
	Warning string // app: shown large and red above the text
	Confirm string // the confirm button's label
	Cancel  string // what the prompt says when cancelled or expired
}

// Answers a deletePresenter reports.
const (
	deleteConfirmed = "confirm"
	deleteCancelled = "cancel"
)

// deletePresenter shows p and calls answer once: deleteConfirmed, or
// deleteCancelled on a cancel or an expiry. An error means nothing was shown.
type deletePresenter func(p deletePrompt, answer func(choice string)) error

// PlanDelete returns the delete's plan without changing anything, the agent
// still running. A delete that cannot run (unknown agent, the last agent,
// another setting names it) is refused here.
func (s *liveAgentStarter) PlanDelete(ctx context.Context, id string) (agentrename.DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return agentrename.DeleteResult{}, errShuttingDown
	}
	return s.deleteFiles(ctx, id, true, io.Discard)
}

// Delete deletes agent id in the running process: it stops the agent live,
// then runs the disk delete, writing its progress to out. Only id stops.
//
// A failure before the disk delete changes nothing and the agent runs on. A
// failed disk step stops the delete where it is (each step is all or
// nothing, the journal records the done ones); while foci.toml still names
// the agent it is started again, and the same /agents delete resumes.
func (s *liveAgentStarter) Delete(ctx context.Context, id string, out io.Writer) (agentrename.DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return agentrename.DeleteResult{}, errShuttingDown
	}
	if _, running := s.agents.Get(id); running {
		// The fresh-run refusals, before the agent stops.
		if _, err := s.deleteFiles(ctx, id, true, io.Discard); err != nil {
			return agentrename.DeleteResult{}, err
		}
	}
	// Not running (a failed start, or a resumed delete): only the disk delete.
	inst, slot, running := s.agents.remove(id)
	if running {
		mainLog.Infof("live delete %s: stopping it", id)
		s.teardown(inst)
	}
	res, err := s.deleteFiles(ctx, id, false, out)
	if err != nil {
		return res, s.recoverFailedDelete(id, slot, err)
	}
	mainLog.Infof("live delete %s: done", id)
	return res, nil
}

// recoverFailedDelete starts the agent again after a failed disk delete when
// foci.toml still names it, and returns the error the user sees.
func (s *liveAgentStarter) recoverFailedDelete(id string, slot int, cause error) error {
	fresh, err := s.load()
	if err != nil {
		return fmt.Errorf("%w. foci.toml cannot be read (%v): fix it, then re-run /agents delete %s to resume", cause, err, id)
	}
	if !slices.ContainsFunc(fresh.Agents, func(a config.AgentConfig) bool { return a.ID == id }) {
		return fmt.Errorf("%w. foci.toml no longer names %q, so it does not run: fix the cause and re-run /agents delete %s to finish", cause, id, id)
	}
	if _, running := s.agents.Get(id); running {
		return fmt.Errorf("%w. Nothing was stopped; %q runs on", cause, id)
	}
	if _, err := s.startAt(id, slot); err != nil {
		return fmt.Errorf("%w. %q could not be started again (%v): re-run /agents delete %s to resume, or /restart", cause, id, err, id)
	}
	return fmt.Errorf("%w. %q runs again. Fix the cause and re-run /agents delete %s to resume", cause, id, id)
}

// deleteFiles runs the disk delete, or only its plan.
func (s *liveAgentStarter) deleteFiles(ctx context.Context, id string, planOnly bool, out io.Writer) (agentrename.DeleteResult, error) {
	if s.deleteDisk == nil {
		return agentrename.DeleteResult{}, errors.New("agent delete is not available")
	}
	return s.deleteDisk(ctx, agentrename.DeleteOptions{ID: id, PlanOnly: planOnly, Out: out})
}

// deleteDiskFn is the production disk delete: agentrename.Delete on the
// gateway's own config file.
func deleteDiskFn(configPath string, backends []string) func(context.Context, agentrename.DeleteOptions) (agentrename.DeleteResult, error) {
	return func(ctx context.Context, o agentrename.DeleteOptions) (agentrename.DeleteResult, error) {
		o.ConfigPath = configPath
		o.Backends = backends
		return agentrename.Delete(ctx, o)
	}
}

// agentDeleter is what the command flow needs from the starter (a fake in
// tests).
type agentDeleter interface {
	PlanDelete(ctx context.Context, id string) (agentrename.DeleteResult, error)
	Delete(ctx context.Context, id string, out io.Writer) (agentrename.DeleteResult, error)
}

// newDeleteCommandFn is the gateway's command.AgentDeleteFn. starter is
// late-bound (main builds it after the agent loop). ctx is the gateway's: a
// delete outlives the command that started it.
func newDeleteCommandFn(ctx context.Context, starter func() *liveAgentStarter, connMgr platform.ConnectionManager) command.AgentDeleteFn {
	notify := func(session, text string) {
		route.NotifySessionChat(connMgr, sessionAgent(session), session, text)
	}
	return func(_ context.Context, a command.AgentDeleteArgs) string {
		s := starter()
		if s == nil {
			return "foci is still starting: try again in a moment."
		}
		present := chatDeletePresenter(connMgr, a.Session)
		if a.Source == "app" {
			present = appDeletePresenter(func(text string) { notify(a.Session, text) })
		}
		return deleteFlow(ctx, s, a, present, func(text string) { notify(a.Session, text) })
	}
}

func sessionAgent(key string) string { return session.AgentIDFromAnyKey(key) }

// deleteFlow plans the delete, asks the two confirmations and, after both,
// runs it in a goroutine and posts the result with notify. It returns the
// command's reply.
func deleteFlow(ctx context.Context, d agentDeleter, a command.AgentDeleteArgs, present deletePresenter, notify func(string)) string {
	id := a.ID
	plan, err := d.PlanDelete(ctx, id)
	if err != nil {
		return fmt.Sprintf("Delete of agent %q refused, nothing changed: %v", id, err)
	}
	what := "- " + strings.Join(plan.Summary, "\n- ")
	cancelled := fmt.Sprintf("Cancelled: agent %q was not deleted.", id)
	suffix := fap.NewULID()
	first := deletePrompt{
		ID:      "agent-delete-1-" + suffix,
		Header:  fmt.Sprintf("Delete agent %s?", id),
		Text:    fmt.Sprintf("Delete agent %q (1 of 2)?\n\nIt stops now and these go:\n%s\n\nA backup is kept in %s. You confirm once more after this.", id, what, plan.Backup),
		Confirm: "Continue",
		Cancel:  cancelled,
	}
	second := deletePrompt{
		ID:      "agent-delete-2-" + suffix,
		Header:  fmt.Sprintf("Really delete agent %s?", id),
		Warning: fmt.Sprintf("DELETE AGENT %s", strings.ToUpper(id)),
		Text:    fmt.Sprintf("Last check (2 of 2): delete agent %q now?\n\n%s\n\nIts workspace and transcripts go to the trash; the rest is in the backup only.", id, what),
		Confirm: "Delete " + id,
		Cancel:  cancelled,
	}
	err = present(first, func(choice string) {
		if choice != deleteConfirmed {
			return
		}
		if err := present(second, func(choice string) {
			if choice != deleteConfirmed {
				return
			}
			go runDelete(ctx, d, id, notify)
		}); err != nil {
			notify(fmt.Sprintf("Agent %q was not deleted: the second confirmation could not be shown (%v).", id, err))
		}
	})
	if err != nil {
		return fmt.Sprintf("Delete of agent %q not started, nothing changed: the confirmation could not be shown (%v).", id, err)
	}
	return fmt.Sprintf("Delete agent %q: confirm it twice in the prompts. Nothing changes unless you confirm both; the result is posted to this chat.", id)
}

// runDelete runs the confirmed delete and posts its result.
func runDelete(ctx context.Context, d agentDeleter, id string, notify func(string)) {
	var out bytes.Buffer
	res, err := d.Delete(ctx, id, &out)
	var msg strings.Builder
	if err != nil {
		fmt.Fprintf(&msg, "❌ Delete of agent %q failed: %v", id, err)
		if i := strings.Index(out.String(), "FAILED"); i >= 0 {
			msg.WriteString("\n\n" + strings.TrimSpace(out.String()[i:]))
		}
		mainLog.Warnf("live delete %s failed: %v", id, err)
	} else {
		fmt.Fprintf(&msg, "🏆 Deleted agent %q:", id)
		for _, l := range res.Summary {
			msg.WriteString("\n- " + l)
		}
		if i := strings.Index(out.String(), "Checklist:"); i >= 0 {
			msg.WriteString("\n\n" + strings.TrimSpace(out.String()[i:]))
		}
	}
	notify(msg.String())
}

// appDeletePresenter shows each confirmation as the app-wide dialog
// (app.PresentAppPrompt), with the warning banner when the prompt has one.
// A cancel or an expiry is also posted to the chat (the dialog just closes).
func appDeletePresenter(notify func(string)) deletePresenter {
	return func(p deletePrompt, answer func(string)) error {
		var once sync.Once
		resolve := func(choice string, why string) {
			once.Do(func() {
				if choice != deleteConfirmed {
					notify(why)
				}
				answer(choice)
			})
		}
		buttons := []platform.ButtonChoice{{Label: p.Confirm, Data: deleteConfirmed}, {Label: "Cancel", Data: deleteCancelled}}
		err := app.PresentAppPrompt(p.ID, p.Header, p.Text, p.Warning, buttons, nil, func(data, _ string) {
			go resolve(data, p.Cancel)
		})
		if err != nil {
			return err
		}
		time.AfterFunc(deleteConfirmTimeout, func() {
			if app.CloseAskgwPrompt(p.ID) {
				resolve(deleteCancelled, "Expired: "+strings.TrimPrefix(p.Cancel, "Cancelled: "))
			}
		})
		return nil
	}
}

// chatDeletePresenter shows each confirmation as a button message in the
// requesting session's chat (Telegram, Discord); the answered message is
// edited to say what happened.
func chatDeletePresenter(connMgr platform.ConnectionManager, requester string) deletePresenter {
	return func(p deletePrompt, answer func(string)) error {
		resolve := sessionChatResolver(connMgr, requester, sessionAgent(requester))
		if resolve() == nil {
			return fmt.Errorf("no chat connection for %s", requester)
		}
		text := p.Text
		if p.Warning != "" {
			text = "⚠️ " + p.Warning + " ⚠️\n\n" + text
		}
		buttons := []platform.ButtonChoice{{Label: p.Confirm, Data: deleteConfirmed}, {Label: "Cancel", Data: deleteCancelled}}
		var once sync.Once
		_, err := platform.SendInteractiveMessageWithID(resolve, p.ID, p.Header, text, buttons,
			func(choice platform.ButtonChoice) string {
				result := "Already answered."
				once.Do(func() {
					if choice.Data == deleteConfirmed {
						result = "Confirmed: " + p.Header
						go answer(deleteConfirmed)
						return
					}
					result = p.Cancel
					go answer(deleteCancelled)
				})
				return result
			},
			func() {})
		if err != nil {
			return err
		}
		time.AfterFunc(deleteConfirmTimeout, func() {
			once.Do(func() {
				_ = platform.CancelInteractiveMessage(p.ID, "Expired: "+strings.TrimPrefix(p.Cancel, "Cancelled: "))
				answer(deleteCancelled)
			})
		})
		return nil
	}
}
