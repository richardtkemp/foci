package tools

// send_to_spawner (#2662, Dick 2026-10-30 12:18: "spawns need to be able to
// message their spawner"): every spawn, of any mode, direct or background,
// same-agent or cross-agent, can send a message to the session that spawned
// it while it runs. For a cross-agent spawn that is the caller's session,
// never the target agent's.
//
// Where the message goes:
//   - A background spawn (character with background, clone): into its
//     spawner's session now, as a turn of its own (queued behind a turn
//     already running there, like any agent-to-agent message: #1167). The
//     spawner answers it with spawn_send. A depth leaf that has reported
//     hands its spawns over (#2624); the message follows the result's route,
//     and while that route holds, it waits at the head of the result.
//   - A direct spawn (raw, explore, character without background): its
//     spawner's turn is normally blocked in the tool call that waits for it.
//     A model cannot read anything while one of its tool calls runs (Claude
//     Code folds input only at a tool boundary, and the API loop drains its
//     steer buffer only between tool calls), so the message is kept and
//     heads the spawn's result. If the spawner's session has no turn running
//     (a Claude Code agent that ran the call in a background shell), nothing
//     waits on the call there, so the message goes at once, as for a
//     background spawn.
//
// How a spawn's own session finds its spawn: a direct API one-shot's tool
// calls carry the spawn in their ctx (spawnLink); a direct delegated batch's
// come over the exec bridge with only the batch's session key, which
// directSpawnBatches maps while the batch runs; a background spawn's branch
// key is in the tracker (ForBranch).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/shared/prompts"
)

// spawnLink names a direct spawn: the Spawner that tracks it (the caller's),
// the caller's session it is keyed under, and its id.
type spawnLink struct {
	s      *Spawner
	parent string
	id     string
	info   RunningSpawn // as registered with the tracker
}

type spawnLinkKey struct{}

// withSpawnLink marks ctx as a direct spawn's run.
func withSpawnLink(ctx context.Context, l spawnLink) context.Context {
	return context.WithValue(ctx, spawnLinkKey{}, l)
}

func spawnLinkFrom(ctx context.Context) (spawnLink, bool) {
	l, ok := ctx.Value(spawnLinkKey{}).(spawnLink)
	return l, ok
}

// directSpawnBatches maps a direct spawn's delegated batch session key to its
// spawnLink while the batch runs (runBatchOneShot, via BatchRequest.OnSession).
var directSpawnBatches sync.Map // batch session key -> spawnLink

// turnInFlighter is the optional agent side of the direct-spawn check: whether
// a turn is running on a session now (agent.Agent.IsTurnInFlight).
type turnInFlighter interface {
	IsTurnInFlight(sessionKey string) bool
}

// sendToSpawnerDescription is the tool's text, pinned in
// TestSpawnToolTextIsApproved.
const sendToSpawnerDescription = "Send a message to the session that spawned you, while you run. Only inside a spawn. " +
	"If you run in the background, it reaches your spawner now, and your spawner can answer with spawn_send. " +
	"If your spawner is waiting for your result, it cannot read anything until you finish: the message is kept and heads your result. " +
	"Your final answer still goes back as your result."

const sendToSpawnerMessageParam = "What to tell your spawner."

// SendToSpawnerTool is send_to_spawner.
func (s *Spawner) SendToSpawnerTool() *Tool {
	return &Tool{
		Name:        "send_to_spawner",
		ExecExport:  true,
		StdinParam:  "message",
		Description: sendToSpawnerDescription,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"message": {
					"type": "string",
					"description": "` + sendToSpawnerMessageParam + `"
				}
			},
			"required": ["message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Message string `json:"message"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			if strings.TrimSpace(p.Message) == "" {
				return ToolResult{}, fmt.Errorf("message is required")
			}
			l, direct, ok := s.spawnOf(ctx)
			if !ok {
				return ToolResult{}, fmt.Errorf("send_to_spawner: this session is not a running spawn, so it has no spawner")
			}
			return l.s.messageFromSpawn(l.parent, l.id, direct, SessionKeyFromContext(ctx), p.Message)
		},
	}
}

// spawnOf resolves the running spawn the calling session is: a direct one
// (from ctx or its batch key) or a background one (its branch key). s is the
// Spawner whose tool was called: the spawn's own agent's, which for a
// cross-agent spawn is the target's, not the caller's.
func (s *Spawner) spawnOf(ctx context.Context) (l spawnLink, direct, ok bool) {
	if l, ok := spawnLinkFrom(ctx); ok {
		return l, true, true
	}
	sk := SessionKeyFromContext(ctx)
	if v, ok := directSpawnBatches.Load(sk); ok {
		return v.(spawnLink), true, true
	}
	parent, info, found := s.deps.Tracker.ForBranch(sk)
	if !found || info.Direct {
		return spawnLink{}, false, false
	}
	owner := s
	if a := session.AgentIDFromKey(parent); a != s.deps.AgentID {
		// A spawn run as this agent for another one: the caller's Spawner
		// tracks it and delivers to the caller.
		if owner = spawnerFor(a); owner == nil {
			return spawnLink{}, false, false
		}
	}
	return spawnLink{s: owner, parent: parent, id: info.ID}, false, true
}

// messageFromSpawn delivers text from parent's running spawn id (whose
// session is from) to its spawner, or keeps it for the head of its result.
func (s *Spawner) messageFromSpawn(parent, id string, direct bool, from, text string) (ToolResult, error) {
	tr := s.deps.Tracker
	info, running := tr.runningSpawn(parent, id)
	if !running {
		return ToolResult{}, fmt.Errorf("send_to_spawner: your spawn has ended, so nothing would read this; put it in your final answer")
	}
	owner, keepWhy := parent, ""
	switch {
	case direct && s.callerTurnRunning(parent):
		keepWhy = "your spawner is waiting for your result and cannot read anything until you finish"
	case direct:
		// Nothing is blocked on the call in the spawner's session.
	default:
		r := s.route(parent)
		if r.Hold {
			keepWhy = "your spawner has handed its work up and its parent has not taken it yet"
		}
		owner = r.Owner
	}
	if keepWhy == "" && s.deps.Notifier == nil {
		keepWhy = "this agent has no background delivery"
	}
	if keepWhy != "" {
		if !tr.keepMessage(parent, id, text) {
			return ToolResult{}, fmt.Errorf("send_to_spawner: your spawn has ended, so nothing would read this; put it in your final answer")
		}
		spawnLog.Infof("spawn=%s message to spawner %s kept for its result (%d chars)", id, parent, len(text))
		return TextResult("Kept: " + keepWhy + ". It will head your result."), nil
	}
	s.deps.Notifier.InjectAgentMessage(owner, formatSpawnerMessage(info, direct, text), "", NewAgentOrigin(from, text))
	tr.appendLog(parent, id, "to-spawner", text)
	spawnLog.Infof("spawn=%s message to spawner %s delivered (%d chars)", id, owner, len(text))
	return TextResult("Sent to your spawner (" + owner + ")."), nil
}

// callerTurnRunning reports whether a turn runs on the spawner's session now,
// so that a direct spawn's call may be what it is waiting on. An agent that
// cannot say counts as running: a kept message is late, a sent one could
// arrive as a turn of its own before the result it belongs with.
func (s *Spawner) callerTurnRunning(parent string) bool {
	if s.agentFn == nil {
		return true
	}
	a, ok := s.agentFn().(turnInFlighter)
	if !ok {
		return true
	}
	return a.IsTurnInFlight(parent)
}

// formatSpawnerMessage is what the spawner's session receives from its spawn.
func formatSpawnerMessage(info RunningSpawn, direct bool, text string) string {
	who := "spawn " + info.ID
	if info.Agent != "" {
		who += ", running as " + info.Agent
	}
	answer := "To answer it, use spawn_send " + info.ID + "."
	if direct {
		answer = "It returns its result to the call that started it, and takes no spawn_send."
	}
	note := "[SYSTEM INJECTION — a message from a spawn you started (" + who + "), NOT from your human. Your human has not seen it. " +
		"The spawn is still running. " + answer + " Your reply here goes to your own chat as usual: if your human needs nothing from this, reply [[NO_RESPONSE]].]"
	return prompts.FormatInjectedMessage("MESSAGE FROM YOUR SPAWN "+info.ID, time.Now(), text, note)
}

// keptMessagesHead heads a spawn's result with the messages it kept for its
// spawner ("" when none).
func keptMessagesHead(id string, kept []SpawnLogEntry) string {
	if len(kept) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[MESSAGES FROM YOUR SPAWN] Spawn %s sent you %d message(s) with send_to_spawner while it ran:", id, len(kept))
	for _, m := range kept {
		fmt.Fprintf(&b, "\n\n[%s]\n%s", timeutil.Format(m.At), m.Text)
	}
	b.WriteString("\n\n[ITS RESULT]\n\n")
	return b.String()
}
