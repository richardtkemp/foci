package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// spawnMessagePrefix heads a parent's message to its spawn, so the spawn can
// tell it from its own task text.
const spawnMessagePrefix = "[MESSAGE FROM YOUR PARENT AGENT]\n"

// spawnStatusDefaultLast is how many activity entries spawn_status shows for
// one spawn when the caller does not say.
const spawnStatusDefaultLast = 10

// SendTool is spawn_send (#2523): the parent sends a message to one of its
// clone spawns. A running spawn gets it folded into its current turn where the
// backend can do that (SpawnSteerer), else at its next step; a finished one
// is resumed on its branch with the message as a new turn, and reports one
// more [SPAWN RESULT] when that ends.
func (s *Spawner) SendTool() *Tool {
	return &Tool{
		Name:        "spawn_send",
		ExecExport:  true,
		Positional:  []string{"message"},
		Description: "Send a message to one of your background spawns (from spawn). A running spawn gets it during its current work; a finished one resumes on its branch, with its earlier context, and reports a new [SPAWN RESULT] when done.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "string",
					"description": "The spawn id (spawn-...) or its branch key, as spawn and spawn_status print them."
				},
				"message": {
					"type": "string",
					"description": "What to tell the spawn."
				},
				"timeout": {
					"type": "integer",
					"description": "Seconds a resumed (finished) spawn may run; default: its first run's timeout; at most 86400 (24 hours). Ignored for a running spawn."
				}
			},
			"required": ["id", "message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				ID      string `json:"id"`
				Message string `json:"message"`
				Timeout int    `json:"timeout"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			if p.ID == "" || strings.TrimSpace(p.Message) == "" {
				return ToolResult{}, fmt.Errorf("id and message are required")
			}
			return s.send(ctx, p.ID, p.Message, p.Timeout)
		},
	}
}

func (s *Spawner) send(ctx context.Context, idOrBranch, message string, timeoutSec int) (ToolResult, error) {
	tr := s.deps.Tracker
	// parent is the session the spawn is keyed under: the caller, or the one
	// that handed the spawn over to it (#2624).
	parent, snap, ok := s.lookupForCaller(SessionKeyFromContext(ctx), idOrBranch, 1)
	if !ok {
		return ToolResult{}, fmt.Errorf("no spawn %q from this session (spawn_status lists them; a restart forgets finished spawns)", idOrBranch)
	}
	if snap.Direct {
		return ToolResult{}, fmt.Errorf("spawn %s returns its result directly to the call that started it, so it takes no messages; spawn_stop stops it", snap.ID)
	}
	text := spawnMessagePrefix + message
	agent := s.agentOf(snap.RunningSpawn)
	if agent == nil {
		return ToolResult{}, fmt.Errorf("spawn_send: agent not available")
	}

	if snap.Running {
		if st, ok := agent.(SpawnSteerer); ok && st.SteerTurn(ctx, snap.BranchKey, text) {
			tr.appendLog(parent, snap.ID, "message", "(steered into the running turn) "+message)
			return TextResult(fmt.Sprintf("Delivered to spawn %s: it folds into its current turn.", snap.ID)), nil
		}
		if tr.queue(parent, snap.ID, text) {
			return TextResult(fmt.Sprintf("Queued for spawn %s: it gets the message at its next step, or as a new turn when its current one ends. Its result still comes once, as [SPAWN RESULT].", snap.ID)), nil
		}
		// It finished between the lookup and the queue: resume it instead.
	}

	timeout := snap.Timeout
	if timeoutSec > 0 || timeout <= 0 {
		timeout = resolveSpawnTimeout(timeoutSec, true) // a resumed spawn runs in the background
	}
	return s.resume(ctx, agent, parent, snap.RunningSpawn, text, message, timeout)
}

// resume runs a follow-up turn on a finished spawn's branch: the branch still
// holds the spawn's conversation (a delegated backend resumes its session), so
// the spawn picks up where it left off.
func (s *Spawner) resume(ctx context.Context, agent SpawnAgent, parent string, running RunningSpawn, text, message string, timeout time.Duration) (ToolResult, error) {
	if s.deps.Notifier == nil {
		return ToolResult{}, fmt.Errorf("spawn_send: no background delivery on this agent")
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
	spawnCtx, cancel := s.runContext(ctx, timeout, running, true)
	if !s.deps.Tracker.restart(parent, running.ID, cancel) {
		cancel()
		<-s.sem
		return ToolResult{}, fmt.Errorf("spawn %s started running again meanwhile; send the message again to reach it", running.ID)
	}
	s.deps.Tracker.appendLog(parent, running.ID, "message", "(resumed with) "+message)
	spawnLog.Infof("resume spawn=%s branch=%s parent=%s message=%d chars timeout=%s",
		running.ID, running.BranchKey, parent, len(message), timeout)
	if s.deps.Observer != nil {
		s.deps.Observer.SpawnStarted(parent, running, message)
	}
	pending := fmt.Sprintf("Spawn %s had finished; it is resumed on branch %s with your message (timeout %s). A new [SPAWN RESULT] comes when it ends.",
		running.ID, running.BranchKey, timeout)
	return s.runBackground(ctx, agent, parent, running, []string{text}, spawnCtx, cancel, pending)
}

// StatusTool is spawn_status (#2523): the parent checks on its spawns, on
// demand only. With no id it lists them; with one it shows that spawn's state
// and its latest activity (its input, assistant text, tool calls).
func (s *Spawner) StatusTool() *Tool {
	return &Tool{
		Name:        "spawn_status",
		ExecExport:  true,
		Description: "Check on your background spawns. With no id: list them (running and recently finished). With an id: that spawn's state and its most recent activity — what it was told, what it said, which tools it called.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "string",
					"description": "A spawn id (spawn-...) or its branch key. Omit to list every spawn of this session."
				},
				"last": {
					"type": "integer",
					"description": "How many of the spawn's most recent activity entries to show (default 10)."
				}
			}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				ID   string `json:"id"`
				Last int    `json:"last"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			parent := SessionKeyFromContext(ctx)
			now := time.Now()
			if p.ID == "" {
				return TextResult(formatSpawnList(s.listForCaller(parent), now)), nil
			}
			last := p.Last
			if last <= 0 {
				last = spawnStatusDefaultLast
			}
			_, snap, ok := s.lookupForCaller(parent, p.ID, last)
			if !ok {
				return ToolResult{}, fmt.Errorf("no spawn %q from this session (spawn_status with no id lists them)", p.ID)
			}
			return TextResult(formatSpawnStatus(snap, now)), nil
		},
	}
}

func spawnState(snap SpawnSnapshot, now time.Time) string {
	resumed := ""
	if snap.Resumes > 0 {
		resumed = fmt.Sprintf(", resumed %d time(s) after a foci restart", snap.Resumes)
	}
	if snap.Running {
		st := "running " + spawnAge(now.Sub(snap.Started))
		if snap.Pending > 0 {
			st += fmt.Sprintf(", %d message(s) waiting", snap.Pending)
		}
		return st + resumed
	}
	return fmt.Sprintf("%s %s ago%s", snap.Outcome, spawnAge(now.Sub(snap.Ended)), resumed)
}

// spawnKind describes how a spawn runs: its mode, whether it returns
// directly or runs in the background, and the agent it runs as when that is
// another one (#2662).
func spawnKind(sp SpawnSnapshot) string {
	parts := []string{}
	if sp.Mode != "" {
		parts = append(parts, sp.Mode)
	}
	if sp.Direct {
		parts = append(parts, "direct")
	} else {
		parts = append(parts, "background")
	}
	if sp.Agent != "" {
		parts = append(parts, "as "+sp.Agent)
	}
	return strings.Join(parts, ", ")
}

// spawnWhere is where a spawn runs, for listing: its branch, or nothing for a
// direct spawn (whose session the caller cannot reach).
func spawnWhere(sp SpawnSnapshot) string {
	if sp.BranchKey == "" {
		return ""
	}
	return "  branch " + sp.BranchKey
}

func formatSpawnList(spawns []SpawnSnapshot, now time.Time) string {
	if len(spawns) == 0 {
		return "No spawns from this session."
	}
	var b strings.Builder
	for _, sp := range spawns {
		fmt.Fprintf(&b, "%s  %s  (%s)%s\n  %s\n", sp.ID, spawnState(sp, now), spawnKind(sp), spawnWhere(sp), sp.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatSpawnStatus(snap SpawnSnapshot, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Spawn %s: %s (%s)\n", snap.ID, spawnState(snap, now), spawnKind(snap))
	if snap.BranchKey != "" {
		fmt.Fprintf(&b, "Branch: %s\n", snap.BranchKey)
	}
	fmt.Fprintf(&b, "Task: %s\n", snap.Description)
	if len(snap.Log) == 0 {
		b.WriteString("\nNo activity yet.")
		return b.String()
	}
	fmt.Fprintf(&b, "\nLatest %d activity entr%s, oldest first:\n", len(snap.Log), map[bool]string{true: "y", false: "ies"}[len(snap.Log) == 1])
	for _, e := range snap.Log {
		fmt.Fprintf(&b, "\n[%s ago] %s: %s\n", spawnAge(now.Sub(e.At)), e.Kind, e.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// spawnAge is a coarse duration: 42s, 3m10s, 2h05m.
func spawnAge(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// StopTool is spawn_stop (#2656): the caller stops one of its running spawns,
// of any mode, by id, or all of them. A background spawn then reports
// [SPAWN RESULT] stopped; a direct one's call returns as stopped.
func (s *Spawner) StopTool() *Tool {
	return &Tool{
		Name:        "spawn_stop",
		ExecExport:  true,
		Positional:  []string{"id"},
		Description: "Stop one of your running spawns, of any mode: a background one reports [SPAWN RESULT] as stopped; one that returns directly ends its call as stopped. spawn_status lists them and their ids. id 'all' stops every running spawn of this session.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "string",
					"description": "The spawn id (spawn-...) or its branch key, as spawn_status prints them; 'all' for every running spawn of this session."
				}
			},
			"required": ["id"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				ID string `json:"id"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			id := strings.TrimSpace(p.ID)
			if id == "" {
				return ToolResult{}, fmt.Errorf("id is required (spawn_status lists your spawns)")
			}
			return s.stop(SessionKeyFromContext(ctx), id)
		},
	}
}

func (s *Spawner) stop(caller, id string) (ToolResult, error) {
	tr := s.deps.Tracker
	if id == "all" {
		n := tr.stopAll(caller, stoppedBySpawnStop)
		for _, p := range tr.parents(s.deps.AgentID) {
			if p != caller && s.deps.Route != nil && s.route(p).Owner == caller {
				n += tr.stopAll(p, stoppedBySpawnStop) // spawns handed over to the caller (#2624)
			}
		}
		if n == 0 {
			return TextResult("No running spawns to stop."), nil
		}
		return TextResult(fmt.Sprintf("Stopping %d spawn(s).", n)), nil
	}
	parent, snap, ok := s.lookupForCaller(caller, id, 0)
	if !ok {
		return ToolResult{}, fmt.Errorf("no spawn %q from this session (spawn_status lists them)", id)
	}
	if _, stopped := tr.stopOne(parent, snap.ID); !stopped {
		return TextResult(fmt.Sprintf("Spawn %s is not running (%s).", snap.ID, spawnState(snap, time.Now()))), nil
	}
	if snap.Direct {
		return TextResult(fmt.Sprintf("Stopping spawn %s: its call returns as stopped.", snap.ID)), nil
	}
	return TextResult(fmt.Sprintf("Stopping spawn %s: its [SPAWN RESULT] will say it was stopped.", snap.ID)), nil
}
