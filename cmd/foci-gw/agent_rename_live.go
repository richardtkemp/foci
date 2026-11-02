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
	"foci/internal/convo"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/tools"
)

// agent_rename_live.go: the live stop of one agent and the live rename built
// on it (#2787). The rename runs inside the gateway, so only the renamed
// agent is interrupted: it is stopped like a shutdown stops every agent, but
// alone (stopLive); its id is rewritten on disk in-process
// (agentrename.Run, as the gateway user, which may write foci.toml and
// secrets.toml in place and owns the crontab); and the new id is started
// live in the old one's place (liveAgentStarter.startAt), where the restore
// phases pick up the work the stop left pending (spawns, depth trees,
// undelivered inputs, asks, wakes), now under the new id.

// stopLive tears down one running agent while every other agent runs on: a
// per-agent copy of runShutdown. The agent has already been unregistered, so
// nothing new routes to it. Order, as at shutdown: drain (no new system
// turn, compaction or backend), keepalive stop, wait for the in-flight turns
// (up to timeout), close the backends and MCP servers, then stop its
// platform connections, its inbox and every goroutine of its own context,
// and close its memory stores and conversation log. Durable work it leaves
// (scheduled wakes, undelivered inputs, spawn records) stays on disk for the
// agent's next start.
func (e *agentEnv) stopLive(inst *agentInstance, timeout time.Duration) {
	id := inst.id
	tools.Spawns.BeginAgentStop(id)
	drainAgent(inst)
	gracefulShutdown(map[string]*agentInstance{id: inst}, timeout)
	closeAgentBackends(inst)
	// From here, what the whole shutdown gets from ending the process.
	e.plat.StopAgent(id)
	inst.ag.StopInbox()
	if inst.cancel != nil {
		inst.cancel()
	}
	tools.UnregisterSpawner(id)
	tools.UnregisterDepthManager(id)
	tools.Spawns.ForgetAgent(id)
	app.ClearReminderStore(id, e.mem.forAgent(id).reminders)
	convo.CloseAgent(id)
	e.mem.removeAgent(id)
	mainLog.Infof("agent %q stopped live (the gateway runs on)", id)
}

// agentRenames maps a renamed agent's old id to its new one for the pieces of
// the running gateway that hold an agent id from the startup config (askgw's
// default agent, master_agent): the config file is rewritten, the frozen
// startup config is not (liveapply.go rule).
type agentRenames struct {
	mu sync.RWMutex
	m  map[string]string
}

// add records old → new, and repoints every earlier rename that led to old.
func (r *agentRenames) add(old, new string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]string)
	}
	for k, v := range r.m {
		if v == old {
			r.m[k] = new
		}
	}
	delete(r.m, new) // a rename back: new is a live id again
	r.m[old] = new
}

// forget drops the rename recorded under id: an agent with that id is being
// started (a new agent reusing a renamed one's old id, or a rename back), so
// id names that agent again, not the one it was renamed to.
func (r *agentRenames) forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

// resolve returns the id an agent now runs under ("" stays "").
func (r *agentRenames) resolve(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n, ok := r.m[id]; ok {
		return n
	}
	return id
}

// gwAgentRenames is the process's rename record.
var gwAgentRenames = &agentRenames{}

// renameRequest is one live rename.
type renameRequest struct {
	Old, New string
	Move     bool // move the workspace ~/Old to ~/New (default: keep it)
}

// Plan returns the rename's plan without changing anything, OLD still
// running. A rename that cannot run (bad id, new id in use, a foci.toml that
// would not load) is refused here, before anything stops.
func (s *liveAgentStarter) Plan(ctx context.Context, req renameRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errShuttingDown
	}
	if err := s.checkRename(req); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := s.renameFiles(ctx, req, true, &out); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// checkRename applies the gateway-side refusals of a live rename.
func (s *liveAgentStarter) checkRename(req renameRequest) error {
	if _, ok := s.agents.Get(req.New); ok {
		return fmt.Errorf("agent %q is already running", req.New)
	}
	return nil
}

// Rename renames agent req.Old to req.New in the running process. It writes
// the rename's progress (plan, steps, checklist) to out and returns hint
// lines for the reply. Only req.Old is interrupted.
//
// A failure part-way leaves a state the user can recover from, and the error
// says how: before the disk rename starts nothing has changed and req.Old
// runs on; a failed step stops the rename where it is (each step is all or
// nothing, and the journal records the done ones), and when foci.toml still
// names req.Old the old agent is started again; the same rename resumes.
func (s *liveAgentStarter) Rename(ctx context.Context, req renameRequest, out io.Writer) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errShuttingDown
	}
	if err := s.checkRename(req); err != nil {
		return nil, err
	}
	note := ""
	if s.renameNote != nil {
		note = s.renameNote(req.Old) // before the stop: it asks whether req.Old is the first agent
	}
	inst, running := s.agents.Get(req.Old)
	if running {
		// The plan run is the full fresh-run validation (the new id unused
		// in every store, foci.toml loads after the edit): refuse before
		// the old agent stops.
		if err := s.renameFiles(ctx, req, true, io.Discard); err != nil {
			return nil, err
		}
	}
	slot := -1
	if running {
		_, slot, _ = s.agents.remove(req.Old)
		mainLog.Infof("live rename %s → %s: stopping %s", req.Old, req.New, req.Old)
		s.teardown(inst)
	}
	// Not running: an unfinished rename of this pair is resumed (the
	// journal decides; a fresh rename of an agent that is not in foci.toml
	// is refused by the engine, changing nothing).

	if err := s.renameFiles(ctx, req, false, out); err != nil {
		return nil, s.recoverFailedRename(req, slot, err)
	}
	gwAgentRenames.add(req.Old, req.New)
	if s.renamePlatforms != nil {
		s.renamePlatforms(req.Old, req.New)
	}
	notes, err := s.startAt(req.New, slot)
	if err != nil {
		return nil, fmt.Errorf("the rename on disk is done, but the new agent %q did not start: %w. Start it with /restart", req.New, err)
	}
	mainLog.Infof("live rename %s → %s: done, %s running", req.Old, req.New, req.New)
	if note != "" {
		notes = append(notes, note)
	}
	return notes, nil
}

// recoverFailedRename restarts the old agent after a failed disk rename when
// foci.toml still names it, and returns the error the user sees, saying what
// runs now and how to finish.
func (s *liveAgentStarter) recoverFailedRename(req renameRequest, slot int, cause error) error {
	fresh, err := s.load()
	if err != nil {
		return fmt.Errorf("%w. foci.toml cannot be read (%v): fix it, then re-run /agents rename %s %s to resume", cause, err, req.Old, req.New)
	}
	if !slices.ContainsFunc(fresh.Agents, func(a config.AgentConfig) bool { return a.ID == req.Old }) {
		return fmt.Errorf("%w. foci.toml already names %q, so neither agent runs: fix the cause and re-run /agents rename %s %s to finish", cause, req.New, req.Old, req.New)
	}
	if _, running := s.agents.Get(req.Old); running {
		return fmt.Errorf("%w. Nothing was stopped; %q runs on", cause, req.Old)
	}
	if _, err := s.startAt(req.Old, slot); err != nil {
		return fmt.Errorf("%w. %q could not be started again (%v): re-run /agents rename %s %s to resume, or /restart", cause, req.Old, err, req.Old, req.New)
	}
	return fmt.Errorf("%w. %q runs again under its old id. Fix the cause and re-run /agents rename %s %s to resume", cause, req.Old, req.Old, req.New)
}

// renameFiles runs the disk rename (agentrename.Run), or only its plan.
func (s *liveAgentStarter) renameFiles(ctx context.Context, req renameRequest, planOnly bool, out io.Writer) error {
	if s.renameDisk == nil {
		return errors.New("agent rename is not available")
	}
	return s.renameDisk(ctx, agentrename.Options{Old: req.Old, New: req.New, Move: req.Move, PlanOnly: planOnly, Out: out})
}

// renameDiskFn is the production disk rename: agentrename.Run on the
// gateway's own config file.
func renameDiskFn(configPath string, backends []string) func(context.Context, agentrename.Options) error {
	return func(ctx context.Context, o agentrename.Options) error {
		o.ConfigPath = configPath
		o.Backends = backends
		return agentrename.Run(ctx, o)
	}
}

// renameKeyFn rewrites a session key of agent old to agent new: the rule the
// disk rename applies to every stored key, for the hub's in-memory bindings.
func renameKeyFn(old, new string) func(string) string {
	return func(key string) string { return agentrename.RewriteSessionKey(old, new, key) }
}

// sharedFacetNote is the reply hint for a live rename of the first (default)
// agent while Telegram shared facet bots run: they are bound to that agent
// at boot, so they go on serving the stopped one until a restart.
func sharedFacetNote(cfg *config.Config, agents *agentRegistry) func(old string) string {
	return func(old string) string {
		first, ok := agents.Default()
		if !ok || first.id != old {
			return ""
		}
		if tg := cfg.Platform("telegram"); tg != nil && len(tg.FacetBots) > 0 {
			return fmt.Sprintf("The shared Telegram facet bots were bound to %q at startup and stay bound to it until /restart: send /restart to move them to the renamed agent.", old)
		}
		return ""
	}
}

// renameApprovalTimeout is how long an HTTP-requested rename waits for the
// owner's answer (the config-change default, #479).
const renameApprovalTimeout = configapprove.DefaultTimeout

// newRenameCommandFn is the gateway's command.AgentRenameFn. starter is
// late-bound (main builds it after the agent loop). ctx is the gateway's:
// a rename outlives the command that started it.
func newRenameCommandFn(ctx context.Context, starter func() *liveAgentStarter, agents *agentRegistry, connMgr platform.ConnectionManager) command.AgentRenameFn {
	return func(_ context.Context, a command.AgentRenameArgs) string {
		s := starter()
		if s == nil {
			return "foci is still starting: try again in a moment."
		}
		req := renameRequest{Old: a.Old, New: a.New, Move: a.Move}
		plan, err := s.Plan(ctx, req)
		if err != nil {
			return fmt.Sprintf("Rename %s → %s refused, nothing changed: %v", a.Old, a.New, err)
		}
		run := func() { go runRename(ctx, s, req, a.Session, connMgr) }
		switch {
		case a.FromHTTP:
			if err := presentRenameApproval(agents, connMgr, a, plan, run); err != nil {
				return fmt.Sprintf("Rename %s → %s not sent for approval, nothing changed: %v", a.Old, a.New, err)
			}
			return fmt.Sprintf("Rename %s → %s sent to the user for approval (Approve/Deny in %s's chat). Nothing changes unless they approve; the result is posted to that chat.", a.Old, a.New, session.AgentIDFromAnyKey(a.Session))
		case !a.Yes:
			move := ""
			if a.Move {
				move = " --move-workspace"
			}
			return fmt.Sprintf("%s\n\nOnly %q stops while the rename runs; every other agent carries on. To run it, send:\n/agents rename %s %s%s --yes", strings.TrimSpace(plan), a.Old, a.Old, a.New, move)
		default:
			run()
			return fmt.Sprintf("Renaming %q to %q: %q stops now, for the time the rename takes. The result is posted to this chat.", a.Old, a.New, a.Old)
		}
	}
}

// presentRenameApproval asks the owner to approve an HTTP-requested rename,
// with Approve/Deny buttons in the requesting agent's default chat (the
// chat #479's config approvals use). run starts the rename on Approve.
func presentRenameApproval(agents *agentRegistry, connMgr platform.ConnectionManager, a command.AgentRenameArgs, plan string, run func()) error {
	agentID := session.AgentIDFromAnyKey(a.Session)
	inst, ok := agents.Get(agentID)
	if !ok {
		return fmt.Errorf("the requesting session %q names no running agent", a.Session)
	}
	target := defaultSessionKeyFor(inst.ag, agentID)
	resolve := connResolver(connMgr, target, agentID)
	if resolve() == nil {
		return fmt.Errorf("no chat connection for %s", target)
	}
	id := "rename-" + fap.NewULID()
	summary := fmt.Sprintf("Approve renaming agent %s to %s?", a.Old, a.New)
	// The question leads the body too: Telegram shows the body, not the header.
	text := fmt.Sprintf("%s\nSession %s asks to rename agent %q to %q. Only %q stops while it runs.\n\n%s", summary, a.Session, a.Old, a.New, a.Old, strings.TrimSpace(plan))
	buttons := []platform.ButtonChoice{{Label: "Approve", Data: "approve"}, {Label: "Deny", Data: "deny"}}
	var once sync.Once // the answer or the expiry, whichever comes first
	_, err := platform.SendInteractiveMessageWithID(resolve, id, summary, text, buttons,
		func(choice platform.ButtonChoice) string {
			result := "Already answered."
			once.Do(func() {
				if choice.Data != "approve" {
					result = fmt.Sprintf("Denied: agent %q was not renamed.", a.Old)
					return
				}
				run()
				result = fmt.Sprintf("Approved: renaming %q to %q. The result is posted to this chat.", a.Old, a.New)
			})
			return result
		},
		func() {})
	if err != nil {
		return err
	}
	time.AfterFunc(renameApprovalTimeout, func() {
		once.Do(func() {
			_ = platform.CancelInteractiveMessage(id, fmt.Sprintf("Expired: agent %q was not renamed.", a.Old))
		})
	})
	return nil
}

// runRename runs one live rename and posts its result to the requesting
// session's chat — under the new id when the requester was the renamed
// agent's own session (the chat is the same one, renamed).
func runRename(ctx context.Context, s *liveAgentStarter, req renameRequest, requester string, connMgr platform.ConnectionManager) {
	var out bytes.Buffer
	notes, err := s.Rename(ctx, req, &out)
	var msg strings.Builder
	if err != nil {
		fmt.Fprintf(&msg, "❌ Rename %s → %s failed: %v", req.Old, req.New, err)
		if i := strings.Index(out.String(), "FAILED"); i >= 0 {
			msg.WriteString("\n\n" + strings.TrimSpace(out.String()[i:]))
		}
		mainLog.Warnf("live rename %s → %s failed: %v", req.Old, req.New, err)
	} else {
		fmt.Fprintf(&msg, "🏆 Renamed %q to %q. %q runs live with %q's history; no other agent was interrupted.", req.Old, req.New, req.New, req.Old)
		for _, n := range notes {
			msg.WriteString("\n" + n)
		}
		if i := strings.Index(out.String(), "Checklist:"); i >= 0 {
			msg.WriteString("\n\n" + strings.TrimSpace(out.String()[i:]))
		}
	}
	target, agentID := requester, session.AgentIDFromAnyKey(requester)
	if err == nil && agentID == req.Old {
		target, agentID = renameKeyFn(req.Old, req.New)(requester), req.New
	}
	route.NotifySessionChat(connMgr, agentID, target, msg.String())
}
