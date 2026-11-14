package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file drive the stub over stdin/stdout the way foci does
// and compare the event order with a live CC 2.1.314 stream (#3056, probes
// in clutch notes/3056.md). Each expected sequence lists only the run
// skeleton: session_state_changed, system/init, assistant, result,
// control_request/response.

type wireStub struct {
	t        *testing.T
	stdin    io.WriteCloser
	lines    chan map[string]any
	cmd      *exec.Cmd
	recorder string
}

// startWireStub runs the stub with session-state events on, as foci runs it.
// A non-nil script is what the stub's next turn loads.
func startWireStub(t *testing.T, script *stubScript, env ...string) *wireStub {
	t.Helper()
	wd := filepath.Join(t.TempDir(), "alpha")
	if err := os.Mkdir(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptDir := t.TempDir()
	if script != nil {
		b, err := json.Marshal(script)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scriptDir, "alpha.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorder := filepath.Join(t.TempDir(), "rec.jsonl")
	cmd := exec.Command(os.Args[0], "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	cmd.Dir = wd
	cmd.Env = append(os.Environ(),
		"CCSTUB_TEST_RUN_MAIN=1",
		"CCSTUB_RECORDER="+recorder,
		"CCSTUB_SCRIPT_DIR="+scriptDir,
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1",
		"CLAUDE_CONFIG_DIR="+t.TempDir(),
	)
	cmd.Env = append(cmd.Env, env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &wireStub{t: t, stdin: stdin, lines: make(chan map[string]any, 256), cmd: cmd, recorder: recorder}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				s.lines <- m
			}
		}
		close(s.lines)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	s.send(map[string]any{"type": "control_request", "request_id": "init-1", "request": map[string]any{"subtype": "initialize"}})
	return s
}

func (s *wireStub) send(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write stdin: %v", err)
	}
}

func (s *wireStub) user(uuid, text string) {
	s.t.Helper()
	s.send(map[string]any{"type": "user", "uuid": uuid, "message": map[string]any{"role": "user", "content": text}})
}

// skeleton names one event: "running"/"idle"/"requires_action" for a state
// change, "init[<uuids>]", "assistant", "result[<uuids>]", "control_request",
// "control_response". Anything else is "".
func skeleton(m map[string]any) string {
	uuids := func() string {
		var ids []string
		if l, ok := m["user_message_uuids"].([]any); ok {
			for _, u := range l {
				s, _ := u.(string)
				ids = append(ids, s)
			}
		}
		return "[" + strings.Join(ids, ",") + "]"
	}
	switch m["type"] {
	case "system":
		switch m["subtype"] {
		case "session_state_changed":
			s, _ := m["state"].(string)
			return s
		case "init":
			return "init" + uuids()
		}
	case "assistant":
		return "assistant"
	case "result":
		return "result" + uuids()
	case "control_request", "control_response":
		return m["type"].(string)
	}
	return ""
}

// readUntil collects skeleton events until stop matches one (inclusive).
func (s *wireStub) readUntil(stop string) []string {
	s.t.Helper()
	var got []string
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("stub stdout closed before %q; got %v", stop, got)
			}
			if k := skeleton(m); k != "" {
				got = append(got, k)
				if k == stop {
					return got
				}
			}
		case <-deadline:
			s.t.Fatalf("no %q within 20s; got %v", stop, got)
		}
	}
}

// recorded returns the stub's recorder entries of one kind.
func (s *wireStub) recorded(kind string) []recorderEntry {
	s.t.Helper()
	data, err := os.ReadFile(s.recorder)
	if err != nil {
		s.t.Fatalf("read recorder: %v", err)
	}
	var out []recorderEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e recorderEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func wantSeq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s:\n got  %v\n want %v", what, got, want)
	}
}

// Real CC sends no system/init at the handshake; it sends one at the start of
// every run, after "running", carrying the run's user message uuid.
func TestWire_InitPerRunNotAtHandshake(t *testing.T) {
	s := startWireStub(t, nil)
	wantSeq(t, "handshake", s.readUntil("control_response"), []string{"control_response"})
	s.user("u-a", "first")
	wantSeq(t, "run 1", s.readUntil("idle"), []string{"running", "init[u-a]", "assistant", "result[u-a]", "idle"})
	s.user("u-b", "second")
	wantSeq(t, "run 2", s.readUntil("idle"), []string{"running", "init[u-b]", "assistant", "result[u-b]", "idle"})
	// Sequential messages are two runs: neither is recorded as folded.
	msgs := s.recorded("user_message")
	if len(msgs) != 2 || msgs[0].Folded || msgs[1].Folded {
		t.Errorf("user_message records = %+v; want two, neither folded", msgs)
	}
}

// The initialize response has real CC's shape: request_id inside response,
// subtype success. foci's WaitReady keys on response.request_id.
func TestWire_InitializeResponseShape(t *testing.T) {
	s := startWireStub(t, nil)
	select {
	case m := <-s.lines:
		resp, _ := m["response"].(map[string]any)
		if m["type"] != "control_response" || resp["subtype"] != "success" || resp["request_id"] != "init-1" {
			t.Fatalf("first line = %v; want control_response{response:{subtype:success, request_id:init-1}}", m)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no initialize response")
	}
	// The handshake still records the system prompt foci sent.
	if got := s.recorded("init_system"); len(got) != 1 {
		t.Errorf("init_system records = %+v; want one", got)
	}
}

// A message foci writes while a run is open is folded into that run: real CC
// answers it after the current result with init, assistant and result, and
// sends ONE idle, no second "running".
func TestWire_SteerFoldsIntoOpenRun(t *testing.T) {
	s := startWireStub(t, nil, "CCSTUB_HANG_DURING_TURN=2s")
	s.readUntil("control_response")
	s.user("u-a", "first")
	wantSeq(t, "run start", s.readUntil("assistant"), []string{"running", "init[u-a]", "assistant"})
	s.user("u-b", "steer")
	wantSeq(t, "folded run", s.readUntil("idle"), []string{"result[u-a]", "init[u-b]", "assistant", "result[u-b]", "idle"})
	msgs := s.recorded("user_message")
	if len(msgs) != 2 || msgs[0].Folded || !msgs[1].Folded {
		t.Errorf("user_message records = %+v; want first not folded, steer folded", msgs)
	}
}

// A scripted permission request comes inside the run: requires_action, the
// can_use_tool request, and the run waits for foci's answer, then "running",
// result, idle. A steer written while it waits is folded into the same run,
// which proves the run was still open.
func TestWire_PermissionRequestHoldsRunInRequiresAction(t *testing.T) {
	s := startWireStub(t, &stubScript{
		Text: "asked",
		PermissionRequests: []scriptedPermissionRequest{{
			ToolName: "Bash", Input: map[string]any{"command": "rm /tmp/x"}, RequestID: "perm-1",
		}},
	})
	s.readUntil("control_response")
	s.user("u-a", "please run it")
	wantSeq(t, "until the prompt", s.readUntil("control_request"), []string{"running", "init[u-a]", "assistant", "requires_action", "control_request"})
	s.user("u-b", "steer while waiting")
	s.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": "perm-1", "response": map[string]any{"behavior": "allow"},
	}})
	wantSeq(t, "after the answer", s.readUntil("idle"), []string{"running", "result[u-a]", "init[u-b]", "assistant", "result[u-b]", "idle"})
}

// A late_text injection is a run of its own: running, init with no
// user_message_uuids (as real CC's background-task notification run), assistant,
// result, idle.
func TestWire_LateTextRunHasInitWithoutUUIDs(t *testing.T) {
	s := startWireStub(t, &stubScript{Text: "now", LateText: "later"})
	s.readUntil("control_response")
	s.user("u-a", "go")
	wantSeq(t, "run", s.readUntil("idle"), []string{"running", "init[u-a]", "assistant", "result[u-a]", "idle"})
	wantSeq(t, "late run", s.readUntil("idle"), []string{"running", "init[]", "assistant", "result[]", "idle"})
}
