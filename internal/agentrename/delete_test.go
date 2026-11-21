package agentrename

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/fociroot"
)

// deleteFixture is the rename fixture with master_agent and [askgw]
// default_agent no longer naming bob, so bob can be deleted.
func deleteFixture(t *testing.T) *fixture {
	f := newFixture(t)
	data, err := os.ReadFile(f.cfgPath)
	must(t, err)
	s := strings.Replace(string(data), `master_agent = "bob"`, `master_agent = "bobby"`, 1)
	s = strings.Replace(s, "[askgw]\ndefault_agent = \"bob\"", "[askgw]\ndefault_agent = \"bobby\"", 1)
	must(t, os.WriteFile(f.cfgPath, []byte(s), 0o644))
	return f
}

// fakeTrash moves paths under <root>/trash and records them.
type fakeTrash struct {
	dir   string
	moved []string
	fail  bool
}

func (ft *fakeTrash) move(p string) (string, error) {
	if ft.fail {
		return "", os.ErrPermission
	}
	must := os.MkdirAll(ft.dir, 0o755)
	if must != nil {
		return "", must
	}
	dst := filepath.Join(ft.dir, filepath.Base(p))
	if err := os.Rename(p, dst); err != nil {
		return "", err
	}
	ft.moved = append(ft.moved, p)
	return dst, nil
}

func (f *fixture) del(o DeleteOptions, ft *fakeTrash) (string, DeleteResult, error) {
	var out bytes.Buffer
	o.ConfigPath = f.cfgPath
	if o.ID == "" {
		o.ID = "bob"
	}
	o.Out = &out
	o.Crontab = f.cron
	o.Now = func() time.Time { return time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC) }
	o.Trash = ft.move
	res, err := Delete(context.Background(), o)
	return out.String(), res, err
}

func TestDeleteRefusesWhileAnotherSettingNamesTheAgent(t *testing.T) {
	f := newFixture(t) // master_agent and [askgw] default_agent name bob
	before := hashTree(t, f.root)
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")}
	_, _, err := f.del(DeleteOptions{}, ft)
	if err == nil || !strings.Contains(err.Error(), "master_agent") || !strings.Contains(err.Error(), "default_agent") {
		t.Fatalf("want a refusal naming master_agent and default_agent, got %v", err)
	}
	diffHashes(t, before, hashTree(t, f.root))
}

func TestDeleteRefusesTheLastAgent(t *testing.T) {
	f := deleteFixture(t)
	data, err := os.ReadFile(f.cfgPath)
	must(t, err)
	s := strings.Replace(string(data), "[[agents]]\nid = \"bobby\"\nbackend = \"claude-code\"\n", "", 1)
	s = strings.Replace(s, `"bobby"`, `""`, -1)
	must(t, os.WriteFile(f.cfgPath, []byte(s), 0o644))
	before := hashTree(t, f.root)
	_, _, err = f.del(DeleteOptions{}, &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")})
	if err == nil || !strings.Contains(err.Error(), "last agent") {
		t.Fatalf("want the last-agent refusal, got %v", err)
	}
	diffHashes(t, before, hashTree(t, f.root))
}

func TestDeletePlanOnlyChangesNothing(t *testing.T) {
	f := deleteFixture(t)
	before := hashTree(t, f.root)
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")}
	out, res, err := f.del(DeleteOptions{PlanOnly: true}, ft)
	must(t, err)
	diffHashes(t, before, hashTree(t, f.root))
	if f.cron.installs != 0 || len(ft.moved) != 0 {
		t.Fatalf("plan-only installed a crontab or trashed something")
	}
	for _, want := range []string{"Delete agent \"bob\"", "[agents.bob.telegram] (values not shown)", "remove: 0 * * * * foci send -a bob hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "BOB-ONLY-SECRET") {
		t.Fatalf("the plan shows a secret value:\n%s", out)
	}
	joined := strings.Join(res.Summary, "\n")
	for _, want := range []string{"[[agents]] block", "secrets.toml", "crontab line(s)", "row(s)", "workspace " + filepath.Join(f.root, "bob") + " (to the trash)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary lacks %q:\n%s", want, joined)
		}
	}
}

func TestDeleteRemovesEverythingButTheLedgers(t *testing.T) {
	f := deleteFixture(t)
	ws := filepath.Join(f.root, "bob")
	ccDir := filepath.Join(f.root, ".claude", "projects", fociroot.CCProjectSlug(ws))
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")}
	apiBefore := queryInt(t, filepath.Join(f.data, "api.db"), `SELECT count(*) FROM api_calls WHERE agent_id = 'bob'`)
	out, res, err := f.del(DeleteOptions{}, ft)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// foci.toml: bob's block gone, bobby's kept, and the file loads.
	cfg, err := config.Load(f.cfgPath, nil)
	must(t, err)
	if findAgent(cfg, "bob") != nil || findAgent(cfg, "bobby") == nil {
		t.Fatalf("agents after delete: %+v", cfg.Agents)
	}
	data, _ := os.ReadFile(f.cfgPath)
	if strings.Contains(string(data), "bob's own comment") {
		t.Fatalf("bob's block is still in foci.toml:\n%s", data)
	}

	// secrets.toml: bob's table and list entry gone, everything else kept;
	// the removed lines are in the backup.
	sec, _ := os.ReadFile(f.secretsPath)
	s := string(sec)
	for _, gone := range []string{"[agents.bob.telegram]", "BOB-ONLY-SECRET", `"bob",`} {
		if strings.Contains(s, gone) {
			t.Errorf("secrets.toml still holds %q:\n%s", gone, s)
		}
	}
	for _, kept := range []string{"[agents.bobby.telegram]", "BOBBY-ONLY-SECRET", `allowed_agents = ["bobby"]`, "SECRET-VALUE", `bob = "TOKEN-BOB"`} {
		if !strings.Contains(s, kept) {
			t.Errorf("secrets.toml lost %q:\n%s", kept, s)
		}
	}
	removed, err := os.ReadFile(filepath.Join(res.Backup, "secrets.removed.toml"))
	must(t, err)
	if !strings.Contains(string(removed), "BOB-ONLY-SECRET") {
		t.Errorf("the removed secrets are not in the backup: %s", removed)
	}

	// The crontab: bob's lines gone (the paused one too), bobby's and bob-2's kept.
	for _, gone := range []string{"-a bob hello", "-a bob -mf", "prompts/y.md"} {
		if strings.Contains(f.cron.content, gone) {
			t.Errorf("crontab still holds %q:\n%s", gone, f.cron.content)
		}
	}
	for _, kept := range []string{"-a bobby hello", "-a bob-2 hello"} {
		if !strings.Contains(f.cron.content, kept) {
			t.Errorf("crontab lost %q:\n%s", kept, f.cron.content)
		}
	}

	// Databases: none of bob's rows left, bobby's all kept; ledgers kept.
	state := filepath.Join(f.data, "state.db")
	for _, q := range []string{
		`SELECT count(*) FROM session_index WHERE agent_id = 'bob' OR substr(session_key,1,4) = 'bob/'`,
		`SELECT count(*) FROM session_metadata WHERE substr(session_key,1,4) = 'bob/'`,
		`SELECT count(*) FROM backend_resume_history WHERE substr(session_key,1,4) = 'bob/'`,
		`SELECT count(*) FROM session_archives WHERE substr(session_key,1,4) = 'bob/'`,
		`SELECT count(*) FROM agent_metadata WHERE agent_id = 'bob' OR value = 'bob/c1'`,
		`SELECT count(*) FROM chat_metadata WHERE agent_id = 'bob'`,
	} {
		if n := queryInt(t, state, q); n != 0 {
			t.Errorf("%s: %d", q, n)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM session_index WHERE agent_id = 'bobby'`,
		`SELECT count(*) FROM agent_metadata WHERE agent_id = 'bobby' OR value = 'bobby/c1'`,
		`SELECT count(*) FROM chat_metadata WHERE agent_id = 'bobby'`,
	} {
		if n := queryInt(t, state, q); n == 0 {
			t.Errorf("bobby's rows gone: %s", q)
		}
	}
	frames := filepath.Join(f.data, "app-frames.db")
	if n := queryInt(t, frames, `SELECT count(*) FROM app_frames WHERE agent_id = 'bob'`) + queryInt(t, frames, `SELECT count(*) FROM app_prompts WHERE agent_id = 'bob'`); n != 0 {
		t.Errorf("bob's app frames left: %d", n)
	}
	if n := queryInt(t, frames, `SELECT count(*) FROM app_frames WHERE agent_id = 'bobby'`); n != 1 {
		t.Errorf("bobby's app frames: %d", n)
	}
	defer_ := filepath.Join(f.data, "deferred-sends.db")
	if n := queryInt(t, defer_, `SELECT count(*) FROM deferred_sends WHERE agent_id = 'bob'`); n != 0 {
		t.Errorf("bob's deferred sends left: %d", n)
	}
	if n := queryInt(t, defer_, `SELECT count(*) FROM deferred_sends WHERE agent_id = 'bobby'`); n != 1 {
		t.Errorf("bobby's deferred sends: %d", n)
	}
	if n := queryInt(t, filepath.Join(f.data, "api.db"), `SELECT count(*) FROM api_calls WHERE agent_id = 'bob'`); n != apiBefore || n == 0 {
		t.Errorf("the cost ledger lost bob's rows: %d, was %d", n, apiBefore)
	}
	if n := queryInt(t, filepath.Join(f.data, "usage-history.db"), `SELECT count(*) FROM usage_reports WHERE agent_id = 'bob'`); n == 0 {
		t.Errorf("the usage ledger lost bob's rows")
	}

	// Sessions tree moved into the backup; workspace and transcripts trashed.
	if _, err := os.Stat(filepath.Join(f.sessions, "bob")); !os.IsNotExist(err) {
		t.Errorf("sessions/bob still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Backup, "sessions", "c1", "root.jsonl")); err != nil {
		t.Errorf("sessions tree not in the backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.sessions, "bobby", "c1", "root.jsonl")); err != nil {
		t.Errorf("bobby's sessions touched: %v", err)
	}
	if len(ft.moved) != 2 || ft.moved[0] != ccDir || ft.moved[1] != ws {
		t.Errorf("trashed %v, want [%s %s]", ft.moved, ccDir, ws)
	}
	if _, err := os.Stat(filepath.Join(f.root, "bobby", ".data", "todo.db")); err != nil {
		t.Errorf("bobby's workspace touched: %v", err)
	}
	// Finished: the journal moved into the backup.
	if _, err := os.Stat(filepath.Join(f.data, deleteJournalName)); !os.IsNotExist(err) {
		t.Errorf("journal left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Backup, "journal.json")); err != nil {
		t.Errorf("journal not in the backup: %v", err)
	}
}

func TestDeleteResumesAfterAFailure(t *testing.T) {
	f := deleteFixture(t)
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash"), fail: true}
	out, _, err := f.del(DeleteOptions{}, ft)
	if err == nil || !strings.Contains(out, "FAILED") {
		t.Fatalf("want the trash step to fail, got %v\n%s", err, out)
	}
	// Everything before the trash step is done: bob is out of foci.toml.
	cfg, err := config.Load(f.cfgPath, nil)
	must(t, err)
	if findAgent(cfg, "bob") != nil {
		t.Fatal("bob still in foci.toml after the config step")
	}
	// Another agent's delete is refused while bob's is unfinished.
	if _, _, err := f.del(DeleteOptions{ID: "bobby"}, ft); err == nil || !strings.Contains(err.Error(), "unfinished delete") {
		t.Fatalf("want the unfinished-delete refusal, got %v", err)
	}
	ft.fail = false
	out, _, err = f.del(DeleteOptions{}, ft)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Resuming the delete") {
		t.Errorf("not a resume:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.root, "bob")); !os.IsNotExist(err) {
		t.Errorf("workspace not trashed on resume: %v", err)
	}
}

func TestDeleteDatabaseFailureIsAllOrNothing(t *testing.T) {
	f := deleteFixture(t)
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")}
	state := filepath.Join(f.data, "state.db")
	before := queryInt(t, state, `SELECT count(*) FROM session_index WHERE agent_id = 'bob'`)
	if _, _, err := f.del(DeleteOptions{failDB: "db:state"}, ft); err == nil {
		t.Fatal("simulated failure did not fail")
	}
	if n := queryInt(t, state, `SELECT count(*) FROM session_index WHERE agent_id = 'bob'`); n != before {
		t.Fatalf("a failed database step changed rows: %d, was %d", n, before)
	}
}

func TestDeleteKeepsASharedWorkspace(t *testing.T) {
	f := deleteFixture(t)
	data, err := os.ReadFile(f.cfgPath)
	must(t, err)
	shared := filepath.Join(f.root, "bob")
	s := strings.Replace(string(data), "[[agents]]\nid = \"bobby\"\nbackend = \"claude-code\"\n",
		"[[agents]]\nid = \"bobby\"\nbackend = \"claude-code\"\nworkspace = \""+shared+"\"\n", 1)
	must(t, os.WriteFile(f.cfgPath, []byte(s), 0o644))
	ft := &fakeTrash{dir: filepath.Join(t.TempDir(), "trash")}
	out, res, err := f.del(DeleteOptions{}, ft)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(ft.moved) != 0 {
		t.Fatalf("a shared workspace was trashed: %v", ft.moved)
	}
	if !strings.Contains(strings.Join(res.Summary, "\n"), "KEPT (the workspace "+shared+" is shared with agent \"bobby\")") {
		t.Errorf("summary does not say the workspace is kept:\n%v", res.Summary)
	}
	todo := filepath.Join(shared, ".data", "todo.db")
	if n := queryInt(t, todo, `SELECT count(*) FROM todos WHERE agent_id = 'bob'`); n != 0 {
		t.Errorf("bob's todos left in the kept workspace: %d", n)
	}
	if n := queryInt(t, todo, `SELECT count(*) FROM todos WHERE agent_id = 'bobby'`); n != 1 {
		t.Errorf("bobby's todo gone: %d", n)
	}
}

func TestRemoveSecretsAndCrontabBoundaries(t *testing.T) {
	in := "[http]\nallowed_agents = [\n  \"bob\",\n  \"bobby\",\n]\ndenied_agents = \"bob, bobby\"\nx = [\"bob\"] # bob\n[agents.\"bob\"]\nk = \"v\"\n[agents.bobby]\nk = \"w\"\n"
	got, removed := removeSecrets(in, "bob")
	want := "[http]\nallowed_agents = [\n  \"bobby\",\n]\ndenied_agents = \"bobby\"\nx = [\"bob\"] # bob\n[agents.bobby]\nk = \"w\"\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %q", removed)
	}
	cron := "0 * * * * foci send -a bob x\n1 * * * * foci send -a bobby x\n2 * * * * /home/u/bob/run.sh\n3 * * * * /home/u/bobby/run.sh\n"
	next, rm := removeCrontab(cron, "bob", "/home/u/bob")
	if next != "1 * * * * foci send -a bobby x\n3 * * * * /home/u/bobby/run.sh\n" || len(rm) != 2 {
		t.Fatalf("crontab got:\n%s\nremoved %q", next, rm)
	}
}
