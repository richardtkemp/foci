package tools

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/usercmd"
)

// fakeShellJobs is a ShellJobEnv whose verdict, approvals and runs a test
// drives. Every command Run is given is recorded in runs, so a test can show a
// command was NOT run.
type fakeShellJobs struct {
	verdict ShellJobCheck
	// asked receives each approval request; answers gives Approve its
	// answer (true = approved). Approve also returns when ctx ends.
	asked   chan ShellJobApproval
	answers chan bool
	// runGate, when set, holds each Run until it receives the result to
	// return (or ctx ends); otherwise Run returns result at once.
	runGate chan usercmd.Result
	result  usercmd.Result

	mu       sync.Mutex
	runs     []string
	timeouts []time.Duration
	started  chan string
}

func newFakeShellJobs(v ShellJobVerdict) *fakeShellJobs {
	return &fakeShellJobs{
		verdict: ShellJobCheck{Verdict: v, Reason: "no auto-approve rule of this agent allows the command"},
		asked:   make(chan ShellJobApproval, 4), answers: make(chan bool, 4),
		result:  usercmd.Result{State: usercmd.StateExited, Output: "all green\n"},
		started: make(chan string, 4),
	}
}

func (f *fakeShellJobs) env() *ShellJobEnv {
	return &ShellJobEnv{
		Check: func(string, string) ShellJobCheck { return f.verdict },
		Approve: func(ctx context.Context, req ShellJobApproval) (bool, string) {
			f.asked <- req
			select {
			case ok := <-f.answers:
				if ok {
					return true, "approved"
				}
				return false, "the user denied it"
			case <-ctx.Done():
				return false, "the job was stopped"
			}
		},
		Run: func(ctx context.Context, command string, timeout time.Duration) usercmd.Result {
			f.mu.Lock()
			f.runs = append(f.runs, command)
			f.timeouts = append(f.timeouts, timeout)
			f.mu.Unlock()
			f.started <- command
			if f.runGate == nil {
				return f.result
			}
			select {
			case r := <-f.runGate:
				return r
			case <-ctx.Done():
				return usercmd.Result{State: usercmd.StateCancelled}
			}
		},
	}
}

// nextAsk waits for the next approval request. A command that ran without
// one fails here, saying what ran.
func (f *fakeShellJobs) nextAsk(t *testing.T) ShellJobApproval {
	t.Helper()
	select {
	case req := <-f.asked:
		return req
	case <-time.After(5 * time.Second):
		t.Fatalf("no approval was asked (ran: %v)", f.ran())
		return ShellJobApproval{}
	}
}

// nextRun waits for the next command Run is given.
func (f *fakeShellJobs) nextRun(t *testing.T) string {
	t.Helper()
	select {
	case c := <-f.started:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no command ran")
		return ""
	}
}

func (f *fakeShellJobs) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

// shellJobHarness is a depth tree and a spawner wired as the gateway wires
// them (newReparentHarness), with fake shell-job deps.
type shellJobHarness struct {
	*reparentHarness
	jobs *fakeShellJobs
}

func newShellJobHarness(t *testing.T, v ShellJobVerdict, store *memSpawnStore) *shellJobHarness {
	t.Helper()
	h := &shellJobHarness{reparentHarness: newReparentHarness(t, newScriptedSpawnAgent(0), store), jobs: newFakeShellJobs(v)}
	m := h.depth.m
	h.sp.spawner.deps.InDepthLeaf = func(k string) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		n := m.nodes[k]
		return n != nil && n.LeafID != ""
	}
	h.sp.spawner.deps.ShellJobs = h.jobs.env
	return h
}

var shellJobIDRe = regexp.MustCompile(`Job: (spawn-[0-9a-f]+)`)

// start calls shell_job from session key.
func (h *shellJobHarness) start(t *testing.T, key, name, command, timeout string) (string, error) {
	t.Helper()
	p := map[string]string{"name": name, "command": command}
	if timeout != "" {
		p["timeout"] = timeout
	}
	raw, _ := json.Marshal(p)
	res, err := h.sp.spawner.ShellJobTool().Execute(WithSessionKey(context.Background(), key), raw)
	if err != nil {
		return "", err
	}
	m := shellJobIDRe.FindStringSubmatch(res.Text)
	if m == nil {
		t.Fatalf("no job id in %q", res.Text)
	}
	return m[1], nil
}

func (h *shellJobHarness) mustStart(t *testing.T, key, name, command string) string {
	t.Helper()
	id, err := h.start(t, key, name, command, "")
	if err != nil {
		t.Fatalf("shell_job: %v", err)
	}
	return id
}

// A shell job outside a depth leaf is refused, and nothing runs.
func TestShellJob_RefusedOutsideLeaf(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	_, err := h.start(t, depthRoot, "ci", "echo hi", "")
	if err == nil || !strings.Contains(err.Error(), "only inside a depth leaf") {
		t.Fatalf("err = %v, want the leaf-only refusal", err)
	}
	if r := h.jobs.ran(); len(r) != 0 {
		t.Fatalf("ran %v outside a leaf", r)
	}
	if l := h.sp.tracker.List(depthRoot); len(l) != 0 {
		t.Fatalf("tracked %+v", l)
	}
}

// A command the rules allow runs at once, under the default 1 h timeout, with
// no approval; its [SPAWN RESULT] carries the exit status and output, it is
// listed by spawn_status as a shell job, and its record goes once the leaf
// has taken the result.
func TestShellJob_AllowedRunsAndReports(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result, 1)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	id := h.mustStart(t, leaf, "ci", "gh run watch && gh pr merge")

	if got := h.jobs.nextRun(t); got != "gh run watch && gh pr merge" {
		t.Fatalf("ran %q", got)
	}
	select {
	case req := <-h.jobs.asked:
		t.Fatalf("an allowed command asked the user: %+v", req)
	default:
	}
	if txt, err := h.status(t, leaf, ""); err != nil || !strings.Contains(txt, "ci") || !strings.Contains(txt, "shell, background") || !strings.Contains(txt, "running") {
		t.Fatalf("spawn_status = %q, %v", txt, err)
	}
	if rec, ok := h.sp.store.record(t, id); !ok || !rec.Running || !rec.Shell || rec.Prompt != "gh run watch && gh pr merge" {
		t.Fatalf("running record = %+v (ok %v)", rec, ok)
	}

	h.jobs.runGate <- usercmd.Result{State: usercmd.StateExited, ExitCode: 0, Output: "merged #42\n"}
	d := h.sp.nextDelivery(t)
	if d.session != leaf {
		t.Fatalf("delivered to %s, want the leaf %s", d.session, leaf)
	}
	for _, want := range []string{"[SPAWN RESULT] Shell job ci", id, "exited with status 0", "merged #42"} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("result lacks %q:\n%s", want, d.msg)
		}
	}
	if ts := h.jobs.timeouts; len(ts) != 1 || ts[0] != ShellJobDefaultTimeout {
		t.Errorf("timeouts = %v, want [%s]", ts, ShellJobDefaultTimeout)
	}
	d.done(true)
	if _, ok := h.sp.store.record(t, id); ok {
		t.Error("record kept after the leaf took the result")
	}
	if txt, _ := h.status(t, leaf, "ci"); !strings.Contains(txt, "completed") {
		t.Errorf("spawn_status ci = %q, want completed", txt)
	}
}

// #3286 requirement: a command outside the agent's rules is NOT run without
// the user's approval. It is put to the user; while they have not answered,
// nothing runs; when they deny it, it never runs and the result says so.
func TestShellJob_UnlistedCommandNotRunWithoutApproval(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAsk, nil)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	id := h.mustStart(t, leaf, "wipe", "rm -rf build")

	req := h.jobs.nextAsk(t)
	if req.Command != "rm -rf build" || req.SessionKey != leaf || req.ID != id || req.Timeout != ShellJobDefaultTimeout {
		t.Fatalf("approval request = %+v", req)
	}
	if r := h.jobs.ran(); len(r) != 0 {
		t.Fatalf("ran %v before the user answered", r)
	}
	if txt, _ := h.status(t, leaf, "wipe"); !strings.Contains(txt, "waiting for the user's approval") {
		t.Errorf("spawn_status = %q, want the wait shown", txt)
	}

	h.jobs.answers <- false
	d := h.sp.nextDelivery(t)
	for _, want := range []string{"[SPAWN RESULT] Shell job wipe", "NOT run", "did not approve", "rm -rf build"} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("result lacks %q:\n%s", want, d.msg)
		}
	}
	d.done(true)
	if r := h.jobs.ran(); len(r) != 0 {
		t.Fatalf("a denied command ran: %v", r)
	}
	if txt, _ := h.status(t, leaf, "wipe"); !strings.Contains(txt, "denied") {
		t.Errorf("spawn_status = %q, want denied", txt)
	}
}

// An unlisted command the user approves runs, after the approval.
func TestShellJob_ApprovedCommandRuns(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAsk, nil)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	h.mustStart(t, leaf, "deploy", "make deploy")
	h.jobs.nextAsk(t)
	h.jobs.answers <- true
	if got := h.jobs.nextRun(t); got != "make deploy" {
		t.Fatalf("ran %q", got)
	}
	d := h.sp.nextDelivery(t)
	if !strings.Contains(d.msg, "exited with status 0") {
		t.Fatalf("result = %q", d.msg)
	}
	d.done(true)
}

// Stopping a job that waits for approval withdraws it: the command never runs.
func TestShellJob_StopWhileAwaitingApproval(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAsk, nil)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	h.mustStart(t, leaf, "wipe", "rm -rf build")
	h.jobs.nextAsk(t)

	raw, _ := json.Marshal(map[string]string{"id": "wipe"})
	if _, err := h.sp.spawner.StopTool().Execute(WithSessionKey(context.Background(), leaf), raw); err != nil {
		t.Fatalf("spawn_stop: %v", err)
	}
	d := h.sp.nextDelivery(t)
	if !strings.Contains(d.msg, "stopped") || !strings.Contains(d.msg, "never ran") {
		t.Fatalf("result = %q", d.msg)
	}
	d.done(true)
	if r := h.jobs.ran(); len(r) != 0 {
		t.Fatalf("a stopped, unapproved command ran: %v", r)
	}
}

// spawn_stop kills a running job; its result says it was stopped.
func TestShellJob_StopWhileRunning(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	h.mustStart(t, leaf, "watch", "gh run watch")
	h.jobs.nextRun(t)
	raw, _ := json.Marshal(map[string]string{"id": "watch"})
	if _, err := h.sp.spawner.StopTool().Execute(WithSessionKey(context.Background(), leaf), raw); err != nil {
		t.Fatalf("spawn_stop: %v", err)
	}
	d := h.sp.nextDelivery(t)
	if !strings.Contains(d.msg, "was stopped by its parent agent") {
		t.Fatalf("result = %q", d.msg)
	}
	d.done(true)
}

// A command a rule refuses is never run, and nobody is asked.
func TestShellJob_DeniedCommandRefused(t *testing.T) {
	h := newShellJobHarness(t, ShellJobDeny, nil)
	h.jobs.verdict.Reason = "rule shell_job_bash_detach refuses it"
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	_, err := h.start(t, leaf, "bg", "nohup server", "")
	if err == nil || !strings.Contains(err.Error(), "NOT run") || !strings.Contains(err.Error(), "shell_job_bash_detach") {
		t.Fatalf("err = %v", err)
	}
	if r := h.jobs.ran(); len(r) != 0 {
		t.Fatalf("ran %v", r)
	}
	select {
	case req := <-h.jobs.asked:
		t.Fatalf("asked %+v", req)
	default:
	}
	if l := h.sp.tracker.List(leaf); len(l) != 0 {
		t.Fatalf("tracked %+v", l)
	}
}

// Dick 2026-11-26: "1 h default, 4 h max".
func TestShellJob_Timeout(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	if _, err := h.start(t, leaf, "long", "make soak", "5h"); err == nil || !strings.Contains(err.Error(), "over the 4h0m0s limit") {
		t.Fatalf("5h: err = %v", err)
	}
	if _, err := h.start(t, leaf, "neg", "true", "0"); err == nil {
		t.Fatal("0: no error")
	}
	h.mustStartWith(t, leaf, "secs", "true", "120")
	h.mustStartWith(t, leaf, "dur", "true", "90m")
	for range 2 {
		h.sp.nextDelivery(t).done(true)
	}
	h.jobs.mu.Lock()
	got := append([]time.Duration(nil), h.jobs.timeouts...)
	h.jobs.mu.Unlock()
	want := map[time.Duration]bool{2 * time.Minute: true, 90 * time.Minute: true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("timeouts = %v, want 2m and 90m", got)
	}
	if r := h.jobs.ran(); len(r) != 2 {
		t.Fatalf("ran %v; the refused timeouts must not run", r)
	}
}

func (h *shellJobHarness) mustStartWith(t *testing.T, key, name, command, timeout string) {
	t.Helper()
	if _, err := h.start(t, key, name, command, timeout); err != nil {
		t.Fatalf("shell_job %s: %v", name, err)
	}
}

// A job takes no spawn slot ([tools] max_concurrent_spawns, Dick 2026-11-26:
// "No, separate"): more jobs than slots all run at once.
func TestShellJob_TakesNoSpawnSlot(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result, 3)
	h.sp.spawner.SetMaxConcurrent(1)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	for _, n := range []string{"a", "b", "c"} {
		h.mustStart(t, leaf, n, "echo "+n)
	}
	for range 3 {
		select {
		case <-h.jobs.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 jobs started with one spawn slot", len(h.jobs.ran()))
		}
	}
	held, _, _ := h.sp.spawner.slots.counts()
	if held != 0 {
		t.Errorf("jobs hold %d spawn slot(s)", held)
	}
	for range 3 {
		h.jobs.runGate <- usercmd.Result{State: usercmd.StateExited}
	}
	for range 3 {
		h.sp.nextDelivery(t).done(true)
	}
}

// spawn_send refuses a shell job: it has no model to message.
func TestShellJob_SpawnSendRefused(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result, 1)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	h.mustStart(t, leaf, "watch", "gh run watch")
	h.jobs.nextRun(t)
	raw, _ := json.Marshal(map[string]string{"id": "watch", "message": "hurry"})
	_, err := h.sp.spawner.SendTool().Execute(WithSessionKey(context.Background(), leaf), raw)
	if err == nil || !strings.Contains(err.Error(), "shell job") {
		t.Fatalf("spawn_send err = %v", err)
	}
	h.jobs.runGate <- usercmd.Result{State: usercmd.StateExited}
	h.sp.nextDelivery(t).done(true)
}

// A leaf may report while its job runs; the job's result is then held until
// the leaf's report reaches the fan-in, and goes to the parent, saying which
// leaf started it (#2624).
func TestShellJob_ReportedLeafResultPassesUp(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result, 1)
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.mustStart(t, kids[0], "ci", "gh run watch")
	h.jobs.nextRun(t)
	if err := h.depth.report(t, kids[0], "alpha: waiting on CI"); err != nil {
		t.Fatalf("a leaf with a running job must be able to report: %v", err)
	}
	h.jobs.runGate <- usercmd.Result{State: usercmd.StateExited, ExitCode: 1, Output: "CI failed"}
	rec := waitResult(t, h.sp.store, id)
	if !strings.Contains(rec.Result, "exited with status 1") {
		t.Fatalf("result = %q", rec.Result)
	}
	h.sp.noDelivery(t) // held: the root's fan-in waits on Beta
	if err := h.depth.report(t, kids[1], "beta done"); err != nil {
		t.Fatal(err)
	}
	d := h.sp.nextDelivery(t)
	if d.session != depthRoot {
		t.Fatalf("delivered to %s, want the root", d.session)
	}
	for _, want := range []string{"[REPARENTED SPAWN]", kids[0], "Shell job ci", "CI failed"} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("result lacks %q:\n%s", want, d.msg)
		}
	}
	d.done(true)
}

// A job running when foci restarts is reported lost and never run again
// (Dick 2026-11-26: "Report it lost").
func TestShellJob_LostAfterRestart(t *testing.T) {
	h := newShellJobHarness(t, ShellJobAllow, nil)
	h.jobs.runGate = make(chan usercmd.Result)
	leaf := h.depth.fanout(t, depthRoot, "Alpha")[0]
	id := h.mustStart(t, leaf, "ci", "gh run watch && gh pr merge")
	h.jobs.nextRun(t)
	if rec, ok := h.sp.store.record(t, id); !ok || !rec.Running || !rec.Shell {
		t.Fatalf("record = %+v (ok %v)", rec, ok)
	}

	// The next process: same store, a fresh tracker and spawner.
	next := newShellJobHarness(t, ShellJobAllow, h.sp.store)
	next.sp.spawner.RestoreSpawns()
	d := next.sp.nextDelivery(t)
	if d.session != leaf {
		t.Fatalf("lost report went to %s, want %s", d.session, leaf)
	}
	for _, want := range []string{id, "lost in a foci restart", "shell job", "gh run watch && gh pr merge"} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("lost report lacks %q:\n%s", want, d.msg)
		}
	}
	d.done(true)
	if r := next.jobs.ran(); len(r) != 0 {
		t.Fatalf("the next process re-ran %v", r)
	}
	if _, ok := next.sp.store.record(t, id); ok {
		t.Error("record kept after the lost report was taken")
	}
}

// The end of a long output, from its saved file, with the file's path.
func TestShellJobOutput_TailOfSavedFile(t *testing.T) {
	path := t.TempDir() + "/out.txt"
	body := strings.Repeat("x", 10000) + "THE END"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := shellJobOutput(usercmd.Result{Output: "x", File: path, Size: int64(len(body))})
	if !strings.Contains(got, "THE END") || !strings.Contains(got, path) || strings.Count(got, "x") > shellJobOutputTail {
		t.Fatalf("output = %.200q…", got)
	}
}
