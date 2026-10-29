package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// #2583: the sync never commits a models.jsonl that modelinfo rejects.

const cleanInput = `{"id":"m1","provider":"openrouter","dev":"acme","input_per_1m":1,"output_per_1m":2,"fetched":"2026-09-01"}
`

// syncRepo is a throwaway foci-shaped repo (internal/modelinfo/models.jsonl on
// main) under its own temp dir, so the sync's sibling worktree lands there too.
type syncRepo struct {
	t      *testing.T
	root   string // parent dir: the sync creates <root>/repo-wt-sync-modelinfo
	repo   string
	before []byte
}

func newSyncRepo(t *testing.T, input string) *syncRepo {
	t.Helper()
	// Isolate git from the host's config (hooks, signing, identity).
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "sync test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "sync@test.invalid")
	}
	root := t.TempDir()
	r := &syncRepo{t: t, root: root, repo: filepath.Join(root, "repo"), before: []byte(input)}
	if err := os.MkdirAll(filepath.Join(r.repo, "internal", "modelinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.jsonl(), r.before, 0o644); err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q", "-b", "main")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	return r
}

func (r *syncRepo) jsonl() string {
	return filepath.Join(r.repo, "internal", "modelinfo", "models.jsonl")
}

func (r *syncRepo) git(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.repo}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// assertNothingCommitted checks the refusal left no trace: no sync branch, no
// review worktree, main's file and history unchanged.
func (r *syncRepo) assertNothingCommitted() {
	r.t.Helper()
	if b := strings.TrimSpace(r.git("branch", "--list", "sync-modelinfo-*")); b != "" {
		r.t.Errorf("sync branch left behind: %q", b)
	}
	if wl := r.git("worktree", "list"); strings.Contains(wl, "-wt-sync-modelinfo") {
		r.t.Errorf("review worktree left behind:\n%s", wl)
	}
	if _, err := os.Stat(filepath.Join(r.root, "repo-wt-sync-modelinfo")); !os.IsNotExist(err) {
		r.t.Errorf("review worktree directory exists (stat err %v)", err)
	}
	if n := strings.TrimSpace(r.git("rev-list", "--count", "--all")); n != "1" {
		r.t.Errorf("repo has %s commits, want only the base commit", n)
	}
	if got, _ := os.ReadFile(r.jsonl()); !bytes.Equal(got, r.before) {
		r.t.Errorf("main's models.jsonl changed:\n%s", got)
	}
}

// stubAPI serves an OpenRouter /models response and points the sync at it.
func stubAPI(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	saved := openrouterAPI
	openrouterAPI = srv.URL
	t.Cleanup(func() { openrouterAPI = saved })
}

func runSync(t *testing.T, r *syncRepo) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run([]string{"--repo", r.repo}, &out, &errb)
	return code, out.String(), errb.String()
}

// m1 at a new price (input 1.5 / output 2): the sync appends one row.
const apiM1Repriced = `{"data":[{"id":"acme/m1","name":"M1","pricing":{"prompt":"0.0000015","completion":"0.000002"}}]}`

func TestSyncRefusesGeneratedRejectedRow(t *testing.T) {
	r := newSyncRepo(t, cleanInput)
	stubAPI(t, apiM1Repriced)
	// A row the sync's own filters would never produce, planted after them.
	testHookEntries = func(e []jsonlEntry) []jsonlEntry {
		return append(e, jsonlEntry{ID: "gen-bad", Provider: "openrouter", InputPer1M: 1, OutputPer1M: 1,
			PriceTiers: []PriceTier{{MinPromptTokens: 0, InputPer1M: 2}}, Fetched: "2026-10-27"})
	}
	t.Cleanup(func() { testHookEntries = nil })

	code, _, stderr := runSync(t, r)
	if code == 0 {
		t.Fatalf("exit 0, want non-zero; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, `"gen-bad"`) || !strings.Contains(stderr, "min_prompt_tokens must be > 0") {
		t.Errorf("stderr does not name gen-bad and the tier reason:\n%s", stderr)
	}
	r.assertNothingCommitted()
}

func TestSyncRefusesPreexistingRejectedRow(t *testing.T) {
	for _, tc := range []struct{ name, badLine, want string }{
		{"no id", `{"provider":"openrouter","input_per_1m":1}`, "entry has no id"},
		{"not json", `{"id":"half-written","provider":`, "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSyncRepo(t, cleanInput+tc.badLine+"\n")
			// The API would add one clean new model.
			stubAPI(t, `{"data":[{"id":"acme/m1","name":"M1","pricing":{"prompt":"0.000001","completion":"0.000002"}},`+
				`{"id":"acme/m2","name":"M2","pricing":{"prompt":"0.000003","completion":"0.000004"}}]}`)

			code, _, stderr := runSync(t, r)
			if code == 0 {
				t.Fatalf("exit 0, want non-zero; stderr:\n%s", stderr)
			}
			if !strings.Contains(stderr, "line 2 ("+tc.badLine[:12]) || !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name line 2 and %q:\n%s", tc.want, stderr)
			}
			r.assertNothingCommitted()
		})
	}
}

// Characterisation: a clean file is committed exactly as before #2583.
func TestSyncCommitsCleanFileUnchanged(t *testing.T) {
	r := newSyncRepo(t, cleanInput)
	stubAPI(t, apiM1Repriced)

	code, stdout, stderr := runSync(t, r)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, stderr)
	}
	branch := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.git("branch", "--list", "sync-modelinfo-*")), "+"))
	if branch == "" {
		t.Fatalf("no sync branch; stdout:\n%s", stdout)
	}
	got := r.git("show", branch+":internal/modelinfo/models.jsonl")
	want := cleanInput + `{"id":"m1","provider":"openrouter","dev":"acme","input_per_1m":1.5,"output_per_1m":2,"fetched":"` +
		time.Now().UTC().Format("2006-01-02") + `"}` + "\n"
	if got != want {
		t.Errorf("committed models.jsonl:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(stdout, "changes at "+filepath.Join(r.root, "repo-wt-sync-modelinfo")) {
		t.Errorf("stdout lacks the review worktree path:\n%s", stdout)
	}
}

// One definition of "rejected": the sync refuses exactly what modelinfo's
// loader rejects, row for row, because it asks the loader (no copied rule).
func TestSyncVerdictIsModelinfoVerdict(t *testing.T) {
	for _, line := range []string{
		`{"id":"ok","provider":"openrouter","input_per_1m":1}`,
		`{"id":"ok-tier","provider":"openrouter","input_per_1m":1,"price_tiers":[{"min_prompt_tokens":1}]}`,
		`{"id":"zero-tier","provider":"openrouter","input_per_1m":1,"price_tiers":[{"min_prompt_tokens":0}]}`,
		`{"id":"neg-tier","provider":"openrouter","price_tiers":[{"min_prompt_tokens":-5}]}`,
		`{"provider":"openrouter"}`,
		`not json`,
	} {
		var stderr bytes.Buffer
		refused := checkOutput([]byte(line+"\n"), "summary", &bytes.Buffer{}, &stderr) != 0
		if want := len(modelinfo.ValidateModelsJSONL([]byte(line))) > 0; refused != want {
			t.Errorf("%s: sync refused=%v, modelinfo rejects=%v\n%s", line, refused, want, stderr.String())
		}
	}
}
