package ccstream

// workflow_tail.go — reads a Workflow run's agent transcripts into the ledger
// adapter (#2130).
//
// A Workflow tool call launches a run of many agents in the background. The
// stream announces the whole run with ONE task_started (task_type
// local_workflow, task_id of the run, never of an agent) and ends it with one
// task_notification; the agents themselves never appear on the stream. Their
// transcripts are written under
//
//	~/.claude/projects/<slug>/<session>/subagents/workflows/<run id>/agent-<id>.jsonl
//
// beside agent-<id>.meta.json and the run's journal.jsonl, NOT under
// subagents/ where a subagent's is, so the subagent tail never saw them and
// every call they made fell into the turn-less remainder. The run id is not
// in task_started; the Workflow tool's PostToolUse carries it, with the
// directory itself, in its tool_response:
//
//	{"status":"async_launched","taskId":"wd6wda9q7","taskType":"local_workflow",
//	 "runId":"wf_0bd537f1-cdd","transcriptDir":"/…/subagents/workflows/wf_0bd537f1-cdd",…}
//
// (captured live, CC 2.1.280). That hook fires while the invoking turn is
// still open, so its turn is the one every agent of the run books on, however
// long after that turn the agent writes (the #2111 ruling: subagent spend is
// tied to its invoking turn).
//
// One goroutine per run polls the directory, finds each agent-*.jsonl as it
// appears, and feeds its assistant lines to the adapter. The run itself is an
// open agent in the adapter from launch until every agent's transcript has
// come to rest after the run's task_notification, which holds off the
// remainder while the run is spending (a result mid-run is otherwise a quiet
// point: the tracker does not count a workflow).

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// workflowLaunch is the part of the Workflow tool's tool_response the tail
// needs. status "async_launched" with taskType "local_workflow" is an
// in-process run; "remote_launched" (a remote agent) writes nothing locally,
// and a script that failed to compile returns async_launched with an error
// and no run.
type workflowLaunch struct {
	Status        string `json:"status"`
	TaskType      string `json:"taskType"`
	RunID         string `json:"runId"`
	TranscriptDir string `json:"transcriptDir"`
	Error         string `json:"error"`
}

// startWorkflowTail starts reading the agent transcripts of the Workflow run
// a PostToolUse reports, on the turn open now. agentID is the hook's agent_id:
// "" for the main thread, else the task id of the subagent that invoked the
// Workflow, whose turn the run then books on.
func (b *Backend) startWorkflowTail(p hookScriptOutput) {
	if b.ledger.Load() == nil {
		return // the tail feeds only the ledger adapter
	}
	var w workflowLaunch
	if err := json.Unmarshal([]byte(p.ToolResponse), &w); err != nil {
		// foci-cc-hook caps tool_response at 4KB; a run's response is ~600
		// bytes, so this is a format change. The run's spend still reaches
		// the remainder, but not its turn.
		b.logger().Warnf("workflow tail: NOT started, tool_response unparseable (tuid=%s): %v", p.ToolUseID, err)
		return
	}
	if w.Status != "async_launched" || w.TaskType != taskTypeWorkflow || w.Error != "" || w.TranscriptDir == "" {
		b.logger().Debugf("workflow tail: NOT started, no local run (tuid=%s status=%q task_type=%q run=%q error=%q)",
			p.ToolUseID, w.Status, w.TaskType, w.RunID, w.Error)
		return
	}
	parent := ""
	if p.AgentID != "" {
		parent = b.groupKeyForTask(p.AgentID)
	}
	turn := b.openTurnRowID()
	b.logger().Debugf("workflow tail: starting group=%s run=%s turn=%q parent=%q dir=%s",
		p.ToolUseID, w.RunID, turn, parent, w.TranscriptDir)
	b.subagentTails().startWorkflow(p.ToolUseID, w.TranscriptDir, turn, parent)
}

// workflowRun is one Workflow run whose agent transcripts are being read.
type workflowRun struct {
	group, dir, turn, parent string

	stop     chan struct{} // closed when the run's task has ended
	stopping bool          // stop is closed; guarded by the manager's mu
	kill     chan struct{} // closed at teardown: read once more and exit
	killOnce sync.Once
	done     chan struct{}

	files map[string]*workflowFile // by agent id; the run goroutine's own
	lines int
}

// workflowFile is one agent transcript of a run.
type workflowFile struct {
	key     string // the agent's ledger actor: "<Workflow tool_use id>/<agent id>"
	off     int64
	partial []byte
	atRest  bool // its last conversational record ends the agent's run
}

// expectWorkflow records a local_workflow task_started, so a
// task_notification that outruns the run's PostToolUse is not lost.
func (m *subagentTailManager) expectWorkflow(group string) {
	if m == nil || group == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workflows[group] == nil {
		if _, seen := m.workflowTasks[group]; !seen {
			m.workflowTasks[group] = false
		}
	}
}

// startWorkflow starts reading the run's agent transcripts in dir. Idempotent.
func (m *subagentTailManager) startWorkflow(group, dir, turn, parent string) {
	if m == nil || m.ledgerEvent == nil || group == "" || dir == "" {
		return
	}
	m.mu.Lock()
	if m.workflows[group] != nil {
		m.mu.Unlock()
		return
	}
	w := &workflowRun{group: group, dir: dir, turn: turn, parent: parent,
		stop: make(chan struct{}), kill: make(chan struct{}), done: make(chan struct{}),
		files: map[string]*workflowFile{}}
	m.workflows[group] = w
	if m.workflowTasks[group] {
		// The run's task already ended: read what it wrote and close.
		w.stopping = true
		close(w.stop)
	}
	delete(m.workflowTasks, group)
	m.mu.Unlock()
	// Opened before the goroutine starts, so a result after this point
	// already sees the run as running.
	m.ledgerEvent(ccEvent{kind: ccTailOpened, agent: group, parent: parent, turn: turn})
	go m.runWorkflow(w)
}

// finalizeWorkflow tells group's run that its task has ended: it reads until
// every agent's transcript is at rest (or subagentTailSettle passes) and
// closes. It does not wait: a run has no text to deliver before its end.
// Called for every terminal task_notification; a group that is not a
// workflow is ignored.
func (m *subagentTailManager) finalizeWorkflow(group string) {
	if m == nil || group == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.workflows[group]; w != nil {
		if !w.stopping {
			w.stopping = true
			close(w.stop)
		}
		return
	}
	if _, expected := m.workflowTasks[group]; expected {
		m.workflowTasks[group] = true // ended before its PostToolUse arrived
	}
}

// stopWorkflows ends every run at teardown and waits for each to exit.
func (m *subagentTailManager) stopWorkflows(runs map[string]*workflowRun) {
	for _, w := range runs {
		w.killOnce.Do(func() { close(w.kill) })
	}
	for _, w := range runs {
		<-w.done
	}
}

func (m *subagentTailManager) runWorkflow(w *workflowRun) {
	defer func() {
		m.mu.Lock()
		if m.workflows[w.group] == w {
			delete(m.workflows, w.group)
		}
		m.mu.Unlock()
		close(w.done)
	}()
	stop := w.stop
	var settle <-chan time.Time
	settled := true
loop:
	for {
		m.scanWorkflow(w)
		if stop == nil && w.atRest() {
			break
		}
		select {
		case <-w.kill:
			m.scanWorkflow(w)
			break loop
		case <-stop:
			stop = nil
			settle = time.After(subagentTailSettle)
		case <-settle:
			settled = false
			break loop
		case <-time.After(subagentTailPoll):
		}
	}
	for _, id := range slices.Sorted(maps.Keys(w.files)) {
		m.ledgerEvent(ccEvent{kind: ccTailClosed, agent: w.files[id].key})
	}
	m.ledgerEvent(ccEvent{kind: ccTailClosed, agent: w.group})
	m.lg.Debugf("workflow tail: closed group=%s agents=%d lines=%d at_rest=%v", w.group, len(w.files), w.lines, settled)
}

// atRest reports whether every agent transcript found ends at a record that
// ends its agent's run.
func (w *workflowRun) atRest() bool {
	for _, f := range w.files {
		if !f.atRest {
			return false
		}
	}
	return true
}

// scanWorkflow finds the run's agent transcripts and reads what each has
// appended since the last scan.
func (m *subagentTailManager) scanWorkflow(w *workflowRun) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return // not created yet
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue // journal.jsonl, the .meta.json sidecars
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		f := w.files[id]
		if f == nil {
			f = &workflowFile{key: w.group + "/" + id}
			w.files[id] = f
			m.ledgerEvent(ccEvent{kind: ccTailOpened, agent: f.key, parent: w.group, turn: w.turn})
		}
		if info, err := e.Info(); err != nil || info.Size() <= f.off {
			continue
		}
		m.readWorkflowFile(w, filepath.Join(w.dir, name), f)
	}
}

// readWorkflowFile feeds f's newly appended complete lines to the adapter.
func (m *subagentTailManager) readWorkflowFile(w *workflowRun, path string, f *workflowFile) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		m.lg.Warnf("workflow tail: seek %s to %d: %v", path, f.off, err)
		return
	}
	data, err := io.ReadAll(fh)
	if err != nil {
		m.lg.Warnf("workflow tail: read %s: %v", path, err)
	}
	f.off += int64(len(data))
	f.partial = append(f.partial, data...)
	for {
		i := bytes.IndexByte(f.partial, '\n')
		if i < 0 {
			return // a torn line waits for the rest
		}
		line := bytes.TrimSpace(f.partial[:i])
		f.partial = f.partial[i+1:]
		if len(line) == 0 {
			continue
		}
		w.lines++
		if l, _ := parseCCRecord(line); l != nil {
			m.ledgerEvent(ccEvent{kind: ccSubLine, agent: f.key, turn: w.turn, line: l})
		}
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				StopReason *string `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Type {
		case "user":
			f.atRest = false
		case "assistant":
			f.atRest = rec.Message.StopReason != nil && *rec.Message.StopReason != "tool_use"
		}
	}
}
