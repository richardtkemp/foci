package secretsapprove

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/secrets"
)

const secretValue = "sk-TEST-0123456789abcdef"

// newStore loads a root store over a temp secrets.toml holding content.
func newStore(t *testing.T, content string) (*secrets.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := secrets.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return st, path
}

// harness is a service whose Present records the prompt instead of showing it.
type harness struct {
	svc     *Service
	store   *secrets.Store
	path    string
	mu      sync.Mutex
	prompts []Prompt
	closed  []string
	results chan Result
}

func newHarness(t *testing.T, content string, presentErr error, timeout time.Duration) *harness {
	t.Helper()
	st, path := newStore(t, content)
	h := &harness{store: st, path: path, results: make(chan Result, 4)}
	h.svc = New(Deps{
		Store:      st,
		KnownAgent: func(id string) bool { return id == "clutch" || id == "scout" },
		Present: func(p Prompt) error {
			if presentErr != nil {
				return presentErr
			}
			h.mu.Lock()
			h.prompts = append(h.prompts, p)
			h.mu.Unlock()
			return nil
		},
		Close:   func(id string) { h.mu.Lock(); h.closed = append(h.closed, id); h.mu.Unlock() },
		Timeout: timeout,
	})
	return h
}

func (h *harness) submit(t *testing.T, req Request) (string, error) {
	t.Helper()
	return h.svc.Submit("clutch", req, func(r Result) { h.results <- r })
}

func (h *harness) prompt(t *testing.T) Prompt {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(h.prompts))
	}
	return h.prompts[0]
}

func (h *harness) result(t *testing.T) Result {
	t.Helper()
	select {
	case r := <-h.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	return Result{}
}

func mustParse(t *testing.T, action string, args []string, hosts, agents string) Request {
	t.Helper()
	r, err := Parse(action, args, hosts, agents)
	if err != nil {
		t.Fatalf("Parse(%s %v): %v", action, args, err)
	}
	return r
}

// The ticket's example end to end: the dialog names the secret, hosts and
// agents; Approve with a typed value writes all three to secrets.toml; the
// agent's result never holds the value.
func TestSet_ApproveWritesValueHostsAgents(t *testing.T) {
	h := newHarness(t, "[other]\nk = \"v\"\n", nil, time.Minute)
	req := mustParse(t, "set", []string{"hetzner.api_key"}, "api.hetzner.cloud,Robot-WS.your-server.de", "clutch,scout")
	if _, err := h.submit(t, req); err != nil {
		t.Fatalf("submit: %v", err)
	}
	p := h.prompt(t)
	if !p.NeedsValue || p.ValueLabel != "hetzner.api_key" {
		t.Errorf("prompt NeedsValue=%t label=%q", p.NeedsValue, p.ValueLabel)
	}
	for _, want := range []string{"clutch", "`hetzner`", "`api_key`", "api.hetzner.cloud", "robot-ws.your-server.de", "scout", "new secret"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("dialog text lacks %q:\n%s", want, p.Text)
		}
	}

	p.OnAnswer(Answer{Decision: ChoiceApprove, Value: "  " + secretValue + "\n"})
	res := h.result(t)
	if res.Outcome != Applied {
		t.Fatalf("outcome = %s (%s), want applied", res.Outcome, res.Message)
	}
	if strings.Contains(res.Message, secretValue) {
		t.Fatalf("result message leaks the value: %s", res.Message)
	}

	re, err := secrets.Load(h.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if v, _ := re.Get("hetzner.api_key"); v != secretValue {
		t.Errorf("stored value = %q, want the trimmed typed value", v)
	}
	if got := re.SectionAllowedHosts("hetzner"); !reflect.DeepEqual(got, []string{"api.hetzner.cloud", "robot-ws.your-server.de"}) {
		t.Errorf("hosts = %v", got)
	}
	if got := re.SectionAllowedAgents("hetzner"); !reflect.DeepEqual(got, []string{"clutch", "scout"}) {
		t.Errorf("agents = %v", got)
	}
	if v, _ := re.Get("other.k"); v != "v" {
		t.Errorf("unrelated secret lost: %q", v)
	}
	if h.svc.Pending() != 0 {
		t.Errorf("request still pending")
	}
}

// Deny and the value step's Cancel change nothing and say which it was.
func TestSet_DenyAndCancelChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		decision string
		want     Outcome
	}{{ChoiceDeny, Denied}, {ChoiceCancel, Cancelled}} {
		t.Run(tc.decision, func(t *testing.T) {
			h := newHarness(t, "", nil, time.Minute)
			if _, err := h.submit(t, mustParse(t, "set", []string{"svc.key"}, "", "")); err != nil {
				t.Fatal(err)
			}
			h.prompt(t).OnAnswer(Answer{Decision: tc.decision})
			if res := h.result(t); res.Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s", res.Outcome, tc.want)
			}
			if _, ok := h.store.Get("svc.key"); ok {
				t.Error("secret written despite no approval")
			}
		})
	}
}

// Only the first answer counts.
func TestAnswer_OnlyOnce(t *testing.T) {
	h := newHarness(t, "", nil, time.Minute)
	if _, err := h.submit(t, mustParse(t, "set", []string{"svc.key"}, "", "")); err != nil {
		t.Fatal(err)
	}
	p := h.prompt(t)
	p.OnAnswer(Answer{Decision: ChoiceDeny})
	h.result(t)
	p.OnAnswer(Answer{Decision: ChoiceApprove, Value: secretValue})
	select {
	case r := <-h.results:
		t.Fatalf("second answer produced %v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := h.store.Get("svc.key"); ok {
		t.Error("late approve wrote the secret")
	}
}

// A value secrets.toml cannot hold fails the request with a reason that does
// not quote it, and writes nothing.
func TestSet_BadValueFailsWithoutEcho(t *testing.T) {
	for name, v := range map[string]string{"empty": "   ", "control": "abc\x07def", "inner newline": "abc\ndef"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", nil, time.Minute)
			if _, err := h.submit(t, mustParse(t, "set", []string{"svc.key"}, "", "")); err != nil {
				t.Fatal(err)
			}
			h.prompt(t).OnAnswer(Answer{Decision: ChoiceApprove, Value: v})
			res := h.result(t)
			if res.Outcome != Failed {
				t.Fatalf("outcome = %s, want failed", res.Outcome)
			}
			if strings.TrimSpace(v) != "" && strings.Contains(res.Message, strings.TrimSpace(v)) {
				t.Errorf("message echoes the value: %s", res.Message)
			}
			if _, ok := h.store.Get("svc.key"); ok {
				t.Error("bad value written")
			}
		})
	}
}

// No answer within the timeout: the dialog is withdrawn and the agent hears
// timed_out.
func TestTimeout_ClosesDialog(t *testing.T) {
	h := newHarness(t, "", nil, 20*time.Millisecond)
	id, err := h.submit(t, mustParse(t, "remove", []string{"svc.key"}, "", ""))
	if err == nil {
		t.Fatalf("remove of a missing secret submitted as %s", id)
	}
	h2 := newHarness(t, "[svc]\nkey = \"v\"\n", nil, 20*time.Millisecond)
	id, err = h2.submit(t, mustParse(t, "remove", []string{"svc.key"}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res := h2.result(t); res.Outcome != TimedOut {
		t.Fatalf("outcome = %s, want timed_out", res.Outcome)
	}
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if !reflect.DeepEqual(h2.closed, []string{id}) {
		t.Errorf("closed = %v, want [%s]", h2.closed, id)
	}
	if _, ok := h2.store.Get("svc.key"); !ok {
		t.Error("timed-out remove removed the secret")
	}
}

// No app device: Submit fails at once and nothing stays pending.
func TestSubmit_NoAppFails(t *testing.T) {
	h := newHarness(t, "", errors.New("no foci app device can show it"), time.Minute)
	if _, err := h.submit(t, mustParse(t, "set", []string{"svc.key"}, "", "")); err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("err = %v, want the app refusal", err)
	}
	if h.svc.Pending() != 0 {
		t.Error("refused request left pending")
	}
}

// An agent rule may name only configured agents.
func TestSubmit_UnknownAgentRefused(t *testing.T) {
	h := newHarness(t, "", nil, time.Minute)
	if _, err := h.submit(t, mustParse(t, "set", []string{"svc.key"}, "", "clutch,ghost")); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want unknown agent ghost", err)
	}
	if len(h.prompts) != 0 {
		t.Error("user was asked about an unknown agent")
	}
}

// Every non-set action, approved, changes the file as /secrets would.
func TestSectionOps_Approve(t *testing.T) {
	base := "[svc]\nkey = \"v\"\nother = \"w\"\nallowed_hosts = [\"a.example\"]\nallowed_in_body = [\"key\"]\ndenied_agents = [\"scout\"]\n"
	cases := []struct {
		action string
		args   []string
		check  func(t *testing.T, st *secrets.Store)
	}{
		{"remove", []string{"svc.other"}, func(t *testing.T, st *secrets.Store) {
			if _, ok := st.Get("svc.other"); ok {
				t.Error("not removed")
			}
		}},
		{"hosts", []string{"svc", "add", "B.example"}, func(t *testing.T, st *secrets.Store) {
			if got := st.SectionAllowedHosts("svc"); !reflect.DeepEqual(got, []string{"a.example", "b.example"}) {
				t.Errorf("hosts = %v", got)
			}
		}},
		{"hosts", []string{"svc", "remove", "a.example"}, func(t *testing.T, st *secrets.Store) {
			if got := st.SectionAllowedHosts("svc"); got != nil {
				t.Errorf("hosts = %v", got)
			}
		}},
		{"hosts", []string{"svc", "clear"}, func(t *testing.T, st *secrets.Store) {
			if got := st.SectionAllowedHosts("svc"); got != nil {
				t.Errorf("hosts = %v", got)
			}
		}},
		{"body", []string{"svc", "add", "other"}, func(t *testing.T, st *secrets.Store) {
			if got := st.SectionAllowedInBody("svc"); !reflect.DeepEqual(got, []string{"key", "other"}) {
				t.Errorf("body = %v", got)
			}
		}},
		{"body", []string{"svc", "clear"}, func(t *testing.T, st *secrets.Store) {
			if got := st.SectionAllowedInBody("svc"); got != nil {
				t.Errorf("body = %v", got)
			}
		}},
		{"agents", []string{"svc", "allow", "clutch"}, func(t *testing.T, st *secrets.Store) {
			if st.SectionDeniedAgents("svc") != nil || !reflect.DeepEqual(st.SectionAllowedAgents("svc"), []string{"clutch"}) {
				t.Errorf("allowed=%v denied=%v", st.SectionAllowedAgents("svc"), st.SectionDeniedAgents("svc"))
			}
		}},
		{"agents", []string{"svc", "clear"}, func(t *testing.T, st *secrets.Store) {
			if st.HasAgentRestrictions() {
				t.Error("agent rules left")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(append([]string{tc.action}, tc.args...), " "), func(t *testing.T) {
			h := newHarness(t, base, nil, time.Minute)
			if _, err := h.submit(t, mustParse(t, tc.action, tc.args, "", "")); err != nil {
				t.Fatal(err)
			}
			p := h.prompt(t)
			if p.NeedsValue {
				t.Error("non-set prompt asks for a value")
			}
			p.OnAnswer(Answer{Decision: ChoiceApprove})
			if res := h.result(t); res.Outcome != Applied {
				t.Fatalf("outcome = %s: %s", res.Outcome, res.Message)
			}
			re, err := secrets.Load(h.path)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			tc.check(t, re)
		})
	}
}

// Parse refuses what would corrupt secrets.toml or make no sense.
func TestParse_Refusals(t *testing.T) {
	cases := []struct {
		action string
		args   []string
		hosts  string
	}{
		{"set", nil, ""},
		{"set", []string{"svc.key", "the-value"}, ""}, // the value is never an argument
		{"set", []string{"nodot"}, ""},
		{"set", []string{"svc.allowed_hosts"}, ""},
		{"set", []string{"agents.key"}, ""},
		{"set", []string{"sv c.key"}, ""},
		{"set", []string{"svc]\n[x.key"}, ""},
		{"set", []string{"svc.key"}, "https://api.example.com/x"},
		{"set", []string{"svc.key"}, "*.example.com"},
		{"remove", []string{"svc.key"}, "a.example"}, // flags are set-only
		{"hosts", []string{"svc", "add"}, ""},
		{"hosts", []string{"svc", "frob", "x"}, ""},
		{"body", []string{"svc", "add", "allowed_hosts"}, ""},
		{"agents", []string{"svc", "allow"}, ""},
		{"agents", []string{"svc", "clear", "x"}, ""},
		{"nope", nil, ""},
	}
	for _, tc := range cases {
		if r, err := Parse(tc.action, tc.args, tc.hosts, ""); err == nil {
			t.Errorf("Parse(%s %q hosts=%q) = %+v, want an error", tc.action, tc.args, tc.hosts, r)
		}
	}
}

// list shows names and rules, never a value.
func TestList_NoValues(t *testing.T) {
	st, _ := newStore(t, "[svc]\nkey = \""+secretValue+"\"\nallowed_hosts = [\"a.example\"]\nallowed_agents = [\"clutch\"]\n")
	out := List(st)
	if strings.Contains(out, secretValue) {
		t.Fatalf("list leaks the value:\n%s", out)
	}
	for _, want := range []string{"[svc]", "key", "a.example", "only clutch"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
}

// #3092 (Dick 2026-11-13): no "approve then type the value" paragraph, no
// current state for a section that does not exist yet, the change as a
// table, the big red warning on every dialog, and (13:10) a secret as
// Group and Name rows, not section.key. Only its new-section and Warning
// checks remain: #3096 changed the existing-section layout, which
// TestDescribe_Layout3096 pins whole.
func TestDescribe_Layout3092(t *testing.T) {
	st, _ := newStore(t, "[old]\nk = \"v\"\nallowed_hosts = [\"a.example\"]\n")
	cases := []struct {
		name      string
		req       Request
		want      []string
		forbidden []string
	}{
		{"set, new section",
			mustParse(t, "set", []string{"fresh.key"}, "api.example", ""),
			[]string{"| Group | `fresh` (new) |\n| Name | `key` (new) |", "| Allowed hosts | `api.example` |", "| Agents | every agent |"},
			[]string{"now", "Now", "was", "unchanged", "Approve, then", "fresh.key"}},
		{"set, new section, no hosts",
			mustParse(t, "set", []string{"fresh.key"}, "", ""),
			[]string{"| Allowed hosts | none (http_request cannot use it) |"},
			[]string{"now", "Now", "was", "unchanged"}},
		{"hosts add, new section",
			mustParse(t, "hosts", []string{"fresh", "add", "c.example"}, "", ""),
			[]string{"| Group | `fresh` (new) |", "| Allowed hosts | `c.example` |"},
			[]string{"Now", "none"}},
		{"agents allow, new section",
			mustParse(t, "agents", []string{"fresh", "allow", "scout"}, "", ""),
			[]string{"| Agents | only `scout` |"}, []string{"Now", "every agent"}},
	}
	for _, c := range cases {
		_, text := describe(st, "clutch", c.req)
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: text lacks %q:\n%s", c.name, w, text)
			}
		}
		for _, f := range c.forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s: text holds %q:\n%s", c.name, f, text)
			}
		}
	}

	h := newHarness(t, "", nil, time.Minute)
	if _, err := h.submit(t, mustParse(t, "hosts", []string{"x", "add", "h.example"}, "", "")); err != nil {
		t.Fatal(err)
	}
	if p := h.prompt(t); p.Warning != Warning || Warning == "" {
		t.Errorf("prompt Warning = %q, want %q", p.Warning, Warning)
	}
}

// TestDescribe_Layout3096 pins the whole table of each kind of request (Dick
// 2026-11-13, #3096): no "(unchanged)" label, Before and After columns when
// the request changes something that exists, and in a table that mixes
// changed and unchanged rows the changed rows are bold (the app fades the
// rest).
func TestDescribe_Layout3096(t *testing.T) {
	st, _ := newStore(t, "[old]\nk = \"v\"\nallowed_hosts = [\"a.example\"]\n")
	cases := []struct {
		name  string
		req   Request
		table string
	}{
		{"set, new section",
			mustParse(t, "set", []string{"fresh.key"}, "", ""),
			"| Field | Value |\n|---|---|\n| Group | `fresh` (new) |\n| Name | `key` (new) |\n" +
				"| Allowed hosts | none (http_request cannot use it) |\n| Agents | every agent |\n"},
		{"set, existing section, new name, rules kept",
			mustParse(t, "set", []string{"old.k2"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| **Name** | **none** | **`k2`** |\n" +
				"| Allowed hosts | `a.example` | `a.example` |\n| Agents | every agent | every agent |\n"},
		{"set, existing section, rules replaced",
			mustParse(t, "set", []string{"old.k2"}, "b.example", "scout"),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| **Name** | **none** | **`k2`** |\n" +
				"| **Allowed hosts** | **`a.example`** | **`b.example`** |\n| **Agents** | **every agent** | **only `scout`** |\n"},
		{"set, replace a value",
			mustParse(t, "set", []string{"old.k"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| Name | `k` | `k` |\n" +
				"| **Value** | **current value** | **new value** |\n" +
				"| Allowed hosts | `a.example` | `a.example` |\n| Agents | every agent | every agent |\n"},
		{"remove",
			mustParse(t, "remove", []string{"old.k"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| **Name** | **`k`** | **removed** |\n"},
		{"hosts add, existing section",
			mustParse(t, "hosts", []string{"old", "add", "c.example"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n" +
				"| **Allowed hosts** | **`a.example`** | **`a.example`, `c.example`** |\n"},
		{"hosts add, already there",
			mustParse(t, "hosts", []string{"old", "add", "a.example"}, "", ""),
			"| Field | Value |\n|---|---|\n| Group | `old` |\n| Allowed hosts | `a.example` |\n"},
		{"hosts add, new section",
			mustParse(t, "hosts", []string{"fresh", "add", "c.example"}, "", ""),
			"| Field | Value |\n|---|---|\n| Group | `fresh` (new) |\n| Allowed hosts | `c.example` |\n"},
		{"body add, existing section",
			mustParse(t, "body", []string{"old", "add", "k"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| **Body keys** | **none** | **`k`** |\n"},
		{"agents allow, existing section",
			mustParse(t, "agents", []string{"old", "allow", "scout"}, "", ""),
			"| Field | Before | After |\n|---|---|---|\n| Group | `old` | `old` |\n| **Agents** | **every agent** | **only `scout`** |\n"},
		{"agents allow, new section",
			mustParse(t, "agents", []string{"fresh", "allow", "scout"}, "", ""),
			"| Field | Value |\n|---|---|\n| Group | `fresh` (new) |\n| Agents | only `scout` |\n"},
	}
	for _, c := range cases {
		_, text := describe(st, "clutch", c.req)
		_, table, ok := strings.Cut(text, "\n\n")
		if !ok || table != c.table {
			t.Errorf("%s: table\n%s\nwant\n%s", c.name, table, c.table)
		}
		for _, f := range []string{"unchanged", "(was", "Now"} {
			if strings.Contains(text, f) {
				t.Errorf("%s: text holds %q", c.name, f)
			}
		}
	}
}
