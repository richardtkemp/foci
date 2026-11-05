package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

const shellTestSecret = "sk-app-2812-very-secret"

// shellHarness is a Hub with a real agent running real "!" commands through the
// real inbound path. Envelopes are captured at Enqueue; no inbox is started.
type shellHarness struct {
	h      *Hub
	client *wsClient
	ag     *agent.Agent
	mu     sync.Mutex
	envs   []agent.Envelope
}

func newShellHarness(t *testing.T, limit int, timeout time.Duration) *shellHarness {
	t.Helper()
	h := newTestHub()
	h.blobs = newBlobStore(t.TempDir())
	fs, err := newFrameStore(filepath.Join(t.TempDir(), "frames.db"), 24*time.Hour)
	if err != nil {
		t.Fatalf("newFrameStore: %v", err)
	}
	h.frames = fs

	secretsPath := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(secretsPath, []byte("[brave]\napi_key = \""+shellTestSecret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{AgentID: "ag", MaxResultChars: limit}
	ag.UserShell = &shell.UserRunner{
		Store:          store,
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return int64(limit) },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return timeout },
	}
	s := &shellHarness{h: h, ag: ag}
	ag.SetEnqueueObserver(func(e agent.Envelope) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.envs = append(s.envs, e)
	})
	h.agents["ag"] = &appConn{hub: h, agentID: "ag", agentRef: ag}
	h.agentOrder = append(h.agentOrder, "ag")
	s.client = fakeClientFor(h)
	s.client.deviceID = "dev-1"
	return s
}

func (s *shellHarness) send(text string) {
	s.h.routeUserTurn(s.client, "conv-1", "ag", text, nil, nil, "msg-1", 1, platform.SteerDefault, false)
}

func (s *shellHarness) envelopes() []agent.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Envelope(nil), s.envs...)
}

func (s *shellHarness) waitEnvelope(t *testing.T) agent.Envelope {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if envs := s.envelopes(); len(envs) > 0 {
			return envs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no envelope reached the agent")
	return agent.Envelope{}
}

// sendCancel puts a shell.cancel frame through the real inbound decoder and gate.
func (s *shellHarness) sendCancel(t *testing.T, convID, runID string, seq int) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"t": fap.TypeShellCancel, "id": fmt.Sprintf("cancel-%d", seq), "seq": seq, "v": fap.ProtocolVersion,
		"d": map[string]string{"conversationId": convID, "runId": runID},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h.dispatchInbound(s.client, data)
}

// flushFrames waits until every frame queued so far is in the store (the writer
// drains in order).
func flushFrames(t *testing.T, fs *frameStore) {
	t.Helper()
	done := make(chan struct{})
	fs.AppendThen("flush-conv", "ag", 1, "{}", 0, false, "", func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("frame store did not drain")
	}
}

func shellOf(t *testing.T, d decoded) fap.ShellCommand {
	t.Helper()
	raw, ok := d.d["shell"]
	if !ok {
		t.Fatalf("frame %s has no shell data: %v", d.t, d.d)
	}
	data, _ := json.Marshal(raw)
	var sc fap.ShellCommand
	if err := json.Unmarshal(data, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

// runAndCollect sends a command and returns its running and final frames.
func (s *shellHarness) runAndCollect(t *testing.T, text string) (decoded, decoded, agent.Envelope) {
	t.Helper()
	s.send(text)
	env := s.waitEnvelope(t)
	frames := drain(t, s.client)
	if len(frames) < 2 {
		t.Fatalf("frames = %v, want running and final", types(frames))
	}
	var shells []decoded
	for _, f := range frames {
		if f.t == fap.TypeMessage {
			t.Errorf("a %q frame reached the client: a command must not leave a plain bubble: %v", f.t, f.d)
		}
		if f.t == fap.TypeNotification {
			shells = append(shells, f)
		}
	}
	if len(shells) != 2 {
		t.Fatalf("notification frames = %d (%v), want 2", len(shells), types(frames))
	}
	return shells[0], shells[1], env
}

// H1: one chit from running to done, and a turn whose text has the command, its
// status and all its output.
func TestShellCommand_App_RunsAsOneChitAndStartsTurn(t *testing.T) {
	s := newShellHarness(t, 1000, 10*time.Second)
	running, final, env := s.runAndCollect(t, "!pwd; echo hi")

	rs := shellOf(t, running)
	if rs.State != usercmd.StateRunning || rs.Command != "pwd; echo hi" {
		t.Errorf("running = %+v", rs)
	}
	fs := shellOf(t, final)
	if fs.State != usercmd.StateExited || fs.ExitCode == nil || *fs.ExitCode != 0 || fs.RunID != rs.RunID {
		t.Errorf("final = %+v, running id %s", fs, rs.RunID)
	}
	if running.d["messageId"] != "msg-1" || final.d["messageId"] != "msg-1" {
		t.Errorf("messageIds = %v, %v: the chit must take the user's message row", running.d["messageId"], final.d["messageId"])
	}
	workspace := s.ag.UserShell.WorkDir
	text, _ := final.d["text"].(string)
	if !strings.Contains(text, "hi") || !strings.Contains(text, "$ pwd; echo hi") || !strings.Contains(text, "exit status 0") {
		t.Errorf("final text = %q", text)
	}
	for _, want := range []string{"$ pwd; echo hi", "exit status 0", "\nhi\n", workspace} {
		if !strings.Contains(env.Text, want) {
			t.Errorf("agent text lacks %q: %q", want, env.Text)
		}
	}
	if env.UserID != "dev-1" || env.SessionKey == "" || env.Driver == nil || !env.Automated {
		t.Errorf("envelope = %+v", env)
	}
}

// H3
func TestShellCommand_App_MultiLineIsOneScript(t *testing.T) {
	s := newShellHarness(t, 1000, 10*time.Second)
	_, final, _ := s.runAndCollect(t, "!echo a\necho b")
	if text, _ := final.d["text"].(string); !strings.Contains(text, "a\nb\n") {
		t.Errorf("text = %q", text)
	}
}

// H2: every escape stays a normal message and runs nothing.
func TestShellCommand_App_EscapesStayNormalText(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "h2")
	for _, tc := range []struct {
		name, text     string
		atts           []platform.Attachment
		refs           []fap.AttachmentRef
		transcribeOnly bool
		wantEnqueued   bool
	}{
		{name: "space", text: "! touch " + marker, wantEnqueued: true},
		{name: "lone", text: "!", wantEnqueued: true},
		{name: "attachment", text: "!touch " + marker, atts: []platform.Attachment{{Type: "photo", Data: []byte{1}}}, wantEnqueued: true},
		{name: "unknown blob ref", text: "!touch " + marker, refs: []fap.AttachmentRef{{BlobID: "gone"}}, wantEnqueued: true},
		{name: "transcribe only", text: "!touch " + marker, transcribeOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newShellHarness(t, 1000, 5*time.Second)
			s.h.routeUserTurn(s.client, "conv-1", "ag", tc.text, tc.atts, tc.refs, "msg-1", 1, platform.SteerDefault, tc.transcribeOnly)
			envs := s.envelopes()
			if tc.wantEnqueued && (len(envs) != 1 || envs[0].Text != tc.text) {
				t.Errorf("envelopes = %+v, want the text unchanged", envs)
			}
			for _, f := range drain(t, s.client) {
				if _, isShell := f.d["shell"]; isShell {
					t.Errorf("a shell frame was sent: %v", f.d)
				}
			}
			s.h.shellRuns.mu.Lock()
			running := len(s.h.shellRuns.runs)
			s.h.shellRuns.mu.Unlock()
			if running != 0 {
				t.Error("a command started")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the marker exists: a command ran")
			}
		})
	}
}

// H5
func TestShellCommand_App_SecretTemplateRefused(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	s.send("!echo {{secret:brave.api_key}}")
	if envs := s.envelopes(); len(envs) != 0 {
		t.Fatalf("a refused command reached the agent: %+v", envs)
	}
	frames := drain(t, s.client)
	if len(frames) != 2 || frames[0].t != fap.TypeMessage || frames[1].t != fap.TypeNotification {
		t.Fatalf("frames = %v, want the user's message and the refusal", types(frames))
	}
	if text, _ := frames[1].d["text"].(string); !strings.Contains(text, "refused") {
		t.Errorf("refusal = %q", text)
	}
	if _, isShell := frames[1].d["shell"]; isShell {
		t.Error("a refusal is not a shell chit")
	}
}

func TestShellCommand_App_ScrubsEverywhere(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	running, final, env := s.runAndCollect(t, fmt.Sprintf(`!printf '%%s' %q`, shellTestSecret))
	for name, got := range map[string]string{
		"running text": running.d["text"].(string), "final text": final.d["text"].(string),
		"running command": shellOf(t, running).Command, "final command": shellOf(t, final).Command,
		"agent": env.Text,
	} {
		if strings.Contains(got, shellTestSecret) {
			t.Errorf("%s holds the secret: %q", name, got)
		}
	}
	if !strings.Contains(final.d["text"].(string), "[REDACTED]") {
		t.Errorf("final text = %q", final.d["text"])
	}
}

// H7: over the limit, the frames and their store never hold the output; the blob
// holds all of it, secret-scrubbed; the agent gets the path of the same file.
func TestShellCommand_App_LargeOutputStaysOutOfFrames(t *testing.T) {
	s := newShellHarness(t, 1000, 20*time.Second)
	d := s.h.devices.pair("dev1", "")
	_, final, env := s.runAndCollect(t, fmt.Sprintf(`!seq 1 200000; printf '%%s\n' %q`, shellTestSecret))

	sc := shellOf(t, final)
	if sc.OutputBlobID == "" || sc.OutputBytes == 0 {
		t.Fatalf("final = %+v, want a blob", sc)
	}
	wire, _ := json.Marshal(final.d)
	if strings.Contains(string(wire), "199999") {
		t.Fatal("the final frame holds the output")
	}
	flushFrames(t, s.h.frames)
	stored := s.h.frames.Range("conv-1", 0, 1000)
	if len(stored) == 0 {
		t.Fatal("control: the frame store is empty")
	}
	for _, f := range stored {
		if strings.Contains(f.wire, "199999") {
			t.Fatal("app-frames holds the output")
		}
	}
	if strings.Contains(env.Text, "199999") {
		t.Fatal("the agent got the full output")
	}

	meta, ok := s.h.blobs.get(sc.OutputBlobID)
	if !ok {
		t.Fatal("no blob")
	}
	if !strings.Contains(env.Text, meta.path) {
		t.Errorf("agent text lacks the saved path %s: %q", meta.path, env.Text)
	}
	req := httptest.NewRequest(http.MethodGet, "/app/blob/"+sc.OutputBlobID, nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w := httptest.NewRecorder()
	s.h.ServeBlobGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET blob = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "\n200000\n") {
		t.Errorf("download lacks the last line (%d bytes)", len(body))
	}
	if strings.Contains(body, shellTestSecret) || !strings.Contains(body, "[REDACTED]") {
		t.Error("the download is not secret-scrubbed")
	}
}

func TestShellCommand_App_SmallOutputIsWholeInTheFrame(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	_, final, env := s.runAndCollect(t, "!seq 1 5")
	if sc := shellOf(t, final); sc.OutputBlobID != "" {
		t.Errorf("blob for a small output: %+v", sc)
	}
	if text, _ := final.d["text"].(string); !strings.Contains(text, "1\n2\n3\n4\n5\n") {
		t.Errorf("text = %q", text)
	}
	if !strings.Contains(env.Text, "1\n2\n3\n4\n5\n") {
		t.Errorf("agent text = %q", env.Text)
	}
}

// H8: the cancel frame, through real dispatch, kills the process; the chit and the
// agent both say cancelled; the output after the kill never appears.
func TestShellCommand_App_CancelKillsTheCommand(t *testing.T) {
	s := newShellHarness(t, 1000, time.Minute)
	s.send("!echo before; sleep 300; echo after")

	var running fap.ShellCommand
	frames := drain(t, s.client)
	if len(frames) != 1 {
		t.Fatalf("frames = %v, want the running chit alone", types(frames))
	}
	running = shellOf(t, frames[0])

	// Another conversation's id cannot cancel it.
	s.sendCancel(t, "other", running.RunID, 1)
	time.Sleep(50 * time.Millisecond)
	if envs := s.envelopes(); len(envs) != 0 {
		t.Fatal("a cancel for another conversation ended the command")
	}

	start := time.Now()
	s.sendCancel(t, "conv-1", running.RunID, 2)
	env := s.waitEnvelope(t)
	if time.Since(start) > 5*time.Second {
		t.Errorf("cancel took %s", time.Since(start))
	}
	if !strings.Contains(env.Text, "[cancelled]") || !strings.Contains(env.Text, "\nbefore\n") || strings.Contains(env.Text, "\nafter") {
		t.Errorf("agent text = %q", env.Text)
	}
	var final []decoded
	for _, f := range drain(t, s.client) {
		if f.t == fap.TypeNotification {
			final = append(final, f)
		}
	}
	if len(final) != 1 || shellOf(t, final[0]).State != usercmd.StateCancelled {
		t.Fatalf("final frames = %v", final)
	}
}

func TestShellCommand_App_TimeoutReported(t *testing.T) {
	s := newShellHarness(t, 1000, 200*time.Millisecond)
	_, final, env := s.runAndCollect(t, "!echo early; sleep 30; echo late")
	if sc := shellOf(t, final); sc.State != usercmd.StateTimedOut || sc.ExitCode != nil {
		t.Errorf("final = %+v", sc)
	}
	if !strings.Contains(env.Text, "timed out after 200ms") || !strings.Contains(env.Text, "\nearly\n") || strings.Contains(env.Text, "\nlate") {
		t.Errorf("agent text = %q", env.Text)
	}
}

// H11: a device that replays the conversation later gets the chit's whole history.
func TestShellCommand_App_ReplayGivesASecondDeviceTheFinalState(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	s.runAndCollect(t, "!echo hi")
	second := fakeClientFor(s.h)
	s.h.convs["conv-1"].replayTo(second, 0)
	var last decoded
	for _, f := range drain(t, second) {
		if f.t == fap.TypeNotification {
			last = f
		}
	}
	if sc := shellOf(t, last); sc.State != usercmd.StateExited {
		t.Errorf("replayed final = %+v", sc)
	}
}

// H11/req 10: a command the restart killed ends failed, once, on every device.
func TestShellCommand_App_StartupClosesAnOrphanedRun(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFrameStore(filepath.Join(dir, "frames.db"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHub()
	h.frames = fs
	b := h.ensureBinding(nil, "ag", "conv-1")
	client := fakeClientFor(h)
	b.attach(client)
	fs.AddShellRun(shellRunRow{runID: "run-1", convID: "conv-1", agentID: "ag", messageID: "msg-1", command: "sleep 300"})
	b.send(shellNotification("conv-1", "msg-1", usercmd.RunningText("sleep 300"),
		fap.ShellCommand{RunID: "run-1", State: usercmd.StateRunning, Command: "sleep 300"}))
	drain(t, client)

	// The gateway comes back: a fresh hub over the same store.
	h2 := newTestHub()
	h2.frames = fs
	h2.StartAll(context.Background())
	b2 := h2.convs["conv-1"]
	if b2 == nil {
		t.Fatal("binding not restored")
	}
	flushFrames(t, fs)
	var last decoded
	second := fakeClientFor(h2)
	b2.replayTo(second, 0)
	for _, f := range drain(t, second) {
		if f.t == fap.TypeNotification {
			last = f
		}
	}
	sc := shellOf(t, last)
	if sc.State != usercmd.StateFailed || sc.RunID != "run-1" || last.d["messageId"] != "msg-1" {
		t.Errorf("closed chit = %+v %v", sc, last.d)
	}
	if !strings.Contains(last.d["text"].(string), "restarted") {
		t.Errorf("text = %q", last.d["text"])
	}
	if open := fs.OpenShellRuns(); len(open) != 0 {
		t.Errorf("open runs = %+v", open)
	}
}

// A finished command leaves no row behind for the startup sweep.
func TestShellCommand_App_FinishedRunLeavesNoRow(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	s.runAndCollect(t, "!true")
	flushFrames(t, s.h.frames)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.h.frames.OpenShellRuns()) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("open runs = %+v", s.h.frames.OpenShellRuns())
}

// Req 13: an app with no shell support gets a known frame type whose text is the
// readable result, and decodes it with no knowledge of the shell key.
func TestShellCommand_App_OldClientReadsPlainNotification(t *testing.T) {
	s := newShellHarness(t, 1000, 5*time.Second)
	_, final, _ := s.runAndCollect(t, "!echo hi")
	if final.t != fap.TypeNotification {
		t.Fatalf("frame type = %q", final.t)
	}
	if text, _ := final.d["text"].(string); !strings.Contains(text, "$ echo hi") || !strings.Contains(text, "hi\n") || !strings.Contains(text, "exit status 0") {
		t.Errorf("text = %q", text)
	}
}

func postShellRunnable(h *Hub, tok, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/app/shell/runnable", strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeShellRunnable(w, req)
	return w
}

func runnableBody(convID, word string) string {
	b, _ := json.Marshal(map[string]string{"conversationId": convID, "word": word})
	return string(b)
}

func TestServeShellRunnable(t *testing.T) {
	s := newShellHarness(t, 1<<20, 5*time.Second)
	tok := s.h.devices.pair("rb", "").Token
	s.h.ensureBinding(nil, "ag", "conv-runnable")

	bin := filepath.Join(s.ag.UserShell.WorkDir, "tool-2878")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, conv, word string
		want             bool
	}{
		{"path in the agent's workspace", "conv-runnable", "./tool-2878", true},
		{"on PATH", "conv-runnable", "sh", true},
		{"not found", "conv-runnable", "no-such-command-2878", false},
		{"unknown conversation", "conv-unknown", "sh", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postShellRunnable(s.h, tok, http.MethodPost, runnableBody(tc.conv, tc.word))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d (%s), want 200", w.Code, w.Body.String())
			}
			var got struct {
				Runnable *bool `json:"runnable"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Runnable == nil {
				t.Fatalf("body %q: %v", w.Body.String(), err)
			}
			if *got.Runnable != tc.want {
				t.Errorf("runnable = %v, want %v", *got.Runnable, tc.want)
			}
		})
	}

	// A question must not mint a conversation, and must start nothing.
	s.h.mu.RLock()
	_, minted := s.h.convs["conv-unknown"]
	s.h.mu.RUnlock()
	if minted {
		t.Error("asking about an unknown conversation created a binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.envs) != 0 {
		t.Errorf("the endpoint reached the agent: %d envelopes", len(s.envs))
	}
}

func TestServeShellRunnable_Rejects(t *testing.T) {
	s := newShellHarness(t, 1<<20, 5*time.Second)
	tok := s.h.devices.pair("rb", "").Token
	for _, tc := range []struct {
		name, tok, method, body string
		want                    int
	}{
		{"no token", "", http.MethodPost, runnableBody("c", "sh"), http.StatusUnauthorized},
		{"bad token", "nope", http.MethodPost, runnableBody("c", "sh"), http.StatusForbidden},
		{"GET", tok, http.MethodGet, "", http.StatusMethodNotAllowed},
		{"not JSON", tok, http.MethodPost, "sh", http.StatusBadRequest},
		{"too large", tok, http.MethodPost, runnableBody("c", strings.Repeat("a", maxShellRunnableBody)), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := postShellRunnable(s.h, tc.tok, tc.method, tc.body); w.Code != tc.want {
				t.Errorf("code = %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}
