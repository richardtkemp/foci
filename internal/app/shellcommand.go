package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/usercmd"
)

// userCommandAgent is the part of *agent.Agent a "!cmd" needs (#2812). Optional:
// an agentCore without it (a test fake) leaves '!' text an ordinary message.
type userCommandAgent interface {
	CheckUserCommand(script string) error
	RedactUserCommand(script string) string
	UserCommandRunnable(word string) bool
	RunUserCommand(ctx context.Context, sessionKey, script string, persist func(path string) (string, error)) usercmd.Result
	EnqueueUserCommandResult(base agent.Envelope, res usercmd.Result) bool
}

// shellRunRegistry holds the running commands' cancels, keyed by run id. The zero
// value is ready.
type shellRunRegistry struct {
	mu   sync.Mutex
	runs map[string]shellRunEntry
}

type shellRunEntry struct {
	convID string
	cancel context.CancelFunc
}

func (r *shellRunRegistry) add(runID, convID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = make(map[string]shellRunEntry)
	}
	r.runs[runID] = shellRunEntry{convID: convID, cancel: cancel}
}

func (r *shellRunRegistry) remove(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, runID)
}

// cancel kills a running command of convID. A run id from another conversation is
// ignored like an unknown one.
func (r *shellRunRegistry) cancel(convID, runID string) {
	r.mu.Lock()
	e, ok := r.runs[runID]
	r.mu.Unlock()
	if ok && e.convID == convID {
		e.cancel()
	}
}

// startShellCommand runs a typed "!cmd" for the conversation. It returns false
// when this agent has no "!" support, and the message stays ordinary text. Access:
// a device that reached this conversation is the operator's own (app devices are
// not operator-gated, see command.OperatorPolicies), and an app conversation is
// never a group chat.
func (h *Hub) startShellCommand(conn *appConn, b *convBinding, script, typed, inID, deviceID string, steer platform.SteerPreference) bool {
	ag, ok := conn.agentRef.(userCommandAgent)
	if !ok {
		return false
	}
	if err := ag.CheckUserCommand(script); err != nil {
		// A refusal does not run and does not reach the agent; the user's message
		// stays a message, with the reason under it.
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: inID, Role: "user", Text: ag.RedactUserCommand(typed)})
		b.send(fap.Notification{ConversationID: b.convID, Text: agent.UserCommandRefusalText(err)})
		return true
	}
	if b.depthLeafOf() != nil {
		if ctl := depthController(); ctl != nil {
			ctl.LeafUserMessage(b.key())
		}
	}
	if conn.OnUserMessage != nil {
		conn.OnUserMessage()
	}

	ctx := h.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	runID := fap.NewULID()
	command := ag.RedactUserCommand(script)
	messageID := inID
	if messageID == "" {
		messageID = runID
	}

	// Recorded before the frame goes out: a crash between the two must not leave a
	// chit no startup sweep can close.
	h.frames.AddShellRun(shellRunRow{runID: runID, convID: b.convID, agentID: b.agent(), messageID: messageID, command: command})
	h.shellRuns.add(runID, b.convID, cancel)
	b.send(shellNotification(b.convID, messageID, usercmd.RunningText(command),
		fap.ShellCommand{RunID: runID, State: usercmd.StateRunning, Command: command}))

	base := agent.Envelope{
		SessionKey: b.key(),
		UserID:     deviceID,
		ChatID:     b.chatID,
		Original:   b.convID,
		Driver:     conn,
		Steer:      steer,
		// No Ref: the user's message row has become the chit, which has no delivery
		// ticks.
	}
	safeGo("shell-command", func() {
		defer cancel()
		defer h.shellRuns.remove(runID)

		var blobID string
		persist := func(path string) (string, error) {
			meta, err := h.blobs.adopt(path, fap.MediaDocument, "shell-output.txt", "text/plain; charset=utf-8")
			if err != nil {
				return "", err
			}
			blobID = meta.id
			return meta.path, nil
		}
		res := ag.RunUserCommand(runCtx, b.key(), script, persist)

		if ctx.Err() != nil {
			// The gateway is stopping: the row stays, and startup closes the chit.
			return
		}
		shell := fap.ShellCommand{RunID: runID, State: res.State, Command: res.Command, OutputBlobID: blobID}
		if blobID != "" {
			shell.OutputBytes = res.Size
		}
		if res.State == usercmd.StateExited {
			code := res.ExitCode
			shell.ExitCode = &code
		}
		b.sendThen(shellNotification(b.convID, messageID, res.DisplayText(), shell), func() {
			h.frames.DeleteShellRun(runID)
		})
		if !ag.EnqueueUserCommandResult(base, res) {
			appLog.Warnf("shell command result refused by the agent inbox (conv=%s run=%s)", b.convID, runID)
		}
	})
	return true
}

// maxShellRunnableBody bounds a POST /app/shell/runnable body. A real one holds a
// conversation id and one word; PATH_MAX (4096) fits with room to spare.
const maxShellRunnableBody = 8 << 10

// ServeShellRunnable handles POST /app/shell/runnable (#2878, #2882): can a word
// start a command for the agent that owns a conversation? The app asks before it
// draws the run button on a code block, because only the server has the agent's
// PATH, files and tools. Body: {"conversationId","word"}. Response: {"runnable":bool}. A conversation
// with no binding, or an agent with no "!" support, answers false. Running stays the
// ordinary "!cmd" message: this endpoint never starts anything.
func (h *Hub) ServeShellRunnable(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authBlob(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ConversationID string `json:"conversationId"`
		Word           string `json:"word"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxShellRunnableBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Runnable bool `json:"runnable"`
	}{h.wordRunnable(req.ConversationID, req.Word)})
}

// wordRunnable answers for the agent that owns convID. It never creates a binding:
// a question must not mint a conversation.
func (h *Hub) wordRunnable(convID, word string) bool {
	h.mu.RLock()
	b := h.convs[convID]
	h.mu.RUnlock()
	if b == nil {
		return false
	}
	conn := h.PrimaryBot(b.agent())
	if conn == nil {
		return false
	}
	ag, ok := conn.agentRef.(userCommandAgent)
	return ok && ag.UserCommandRunnable(word)
}

// shellNotification is one state of a shell chit. Ambient: neither state is news
// the user needs a push for (they typed it, and the agent's reply is the news).
func shellNotification(convID, messageID, text string, shell fap.ShellCommand) fap.Notification {
	return fap.Notification{ConversationID: convID, MessageID: messageID, Text: text, Shell: &shell, Ambient: true}
}

// closeOrphanedShellRuns gives each command the last gateway run left unfinished
// its final state (#2812 req 10): the restart killed the process, so its chit must
// not stay "running" for ever on any device.
func (h *Hub) closeOrphanedShellRuns() {
	for _, r := range h.frames.OpenShellRuns() {
		res := usercmd.Result{Command: r.command, State: usercmd.StateFailed, Detail: "the gateway restarted while it ran"}
		b := h.ensureBinding(nil, r.agentID, r.convID)
		runID := r.runID
		b.sendThen(shellNotification(r.convID, r.messageID, res.DisplayText(),
			fap.ShellCommand{RunID: runID, State: res.State, Command: r.command}), func() {
			h.frames.DeleteShellRun(runID)
		})
	}
}
