package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/log"
)

// reloadWarnMarker is the stable substring of the keep-last-good warning;
// counting its occurrences measures how many warnings were emitted.
const reloadWarnMarker = "keeping the last good contents"

// mtimeSeq returns a function producing strictly increasing deterministic
// modification times, so file changes are detectable without sleeping: change
// detection compares stamps for equality, not wall-clock order.
func mtimeSeq() func() time.Time {
	base := time.Unix(1700000000, 0)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * 17 * time.Millisecond)
	}
}

// rewrite replaces the secrets file's contents and sets its modification time
// to a deterministic stamp, so the next store read reliably detects the change
// regardless of filesystem timestamp granularity.
func rewrite(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// captureWarns redirects the log package's event output to a buffer for the
// rest of the test and restores it on cleanup. Tests using it must not run in
// parallel — log output is process-global.
func captureWarns(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })
	return &buf
}

func TestReloadPicksUpChangedValue(t *testing.T) {
	// Proves that a value changed on disk is returned by the very next
	// read on the same store, with no restart, command or timer (#1269).
	path := writeSecrets(t, `[custom]
api_key = "one"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	next := mtimeSeq()
	rewrite(t, path, "[custom]\napi_key = \"two\"\n", next())

	if v, ok := s.Get("custom.api_key"); !ok || v != "two" {
		t.Errorf("Get(custom.api_key) = %q, %v — want \"two\" after the file changed", v, ok)
	}
}

func TestReloadDetectsRenameReplace(t *testing.T) {
	// Proves that an editor which replaces the secrets file by rename
	// (new inode, os.SameFile false) is detected, not just in-place edits.
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(path, []byte("[custom]\nkey = \"old\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tmp := filepath.Join(dir, "secrets.toml.new")
	if err := os.WriteFile(tmp, []byte("[custom]\nkey = \"new\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := mtimeSeq()()
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	if v, ok := s.Get("custom.key"); !ok || v != "new" {
		t.Errorf("Get(custom.key) = %q, %v — want \"new\" after rename-replace", v, ok)
	}
}

func TestReloadDetectsSubSecondMtimeChange(t *testing.T) {
	// Proves that change detection uses the modification time at full
	// resolution: a same-size edit only 17ms newer is picked up.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"aaa\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, "[custom]\nkey = \"bbb\"\n", next()) // same size, +17ms

	if v, ok := s.Get("custom.key"); !ok || v != "bbb" {
		t.Errorf("Get(custom.key) = %q, %v — want \"bbb\" after sub-second mtime change", v, ok)
	}
}

func TestForAgentFollowsFileChange(t *testing.T) {
	// Proves that a per-agent view created BEFORE the file changed sees
	// the new values, host overrides, agent overrides and agent restrictions
	// on its next use — the store pointer tools already hold goes live.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, `
[custom]
shared = "global_v1"

[api]
token = "tok_v1"
allowed_hosts = ["old.example.com"]

[restricted]
secret = "visible_v1"

[agents.alpha.custom]
shared = "alpha_v1"
`, next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := s.ForAgent("alpha")

	if v, _ := view.Get("custom.shared"); v != "alpha_v1" {
		t.Fatalf("setup: view Get(custom.shared) = %q, want alpha_v1", v)
	}

	rewrite(t, path, `
[custom]
shared = "global_v2"

[api]
token = "tok_v2"
allowed_hosts = ["new.example.com"]

[restricted]
secret = "hidden_v2"
allowed_agents = ["beta"]

[agents.alpha.custom]
shared = "alpha_v2"
`, next())

	if v, ok := view.Get("custom.shared"); !ok || v != "alpha_v2" {
		t.Errorf("view Get(custom.shared) = %q, %v — want the new agent override alpha_v2", v, ok)
	}
	if v, ok := view.Get("api.token"); !ok || v != "tok_v2" {
		t.Errorf("view Get(api.token) = %q, %v — want the new global tok_v2", v, ok)
	}
	if hosts := view.AllowedHosts("api.token"); len(hosts) != 1 || hosts[0] != "new.example.com" {
		t.Errorf("view AllowedHosts(api.token) = %v — want [new.example.com]", hosts)
	}
	if _, ok := view.Get("restricted.secret"); ok {
		t.Error("view should not see restricted.secret after allowed_agents excluded alpha")
	}
	// The root keeps its unfiltered view of the same file.
	if v, ok := s.Get("custom.shared"); !ok || v != "global_v2" {
		t.Errorf("root Get(custom.shared) = %q, %v — want global_v2", v, ok)
	}
	if _, ok := s.Get("restricted.secret"); !ok {
		t.Error("root should still see restricted.secret")
	}
}

func TestRootMutationsReachAgentStores(t *testing.T) {
	// Proves that in-process mutations on the root store (the /secrets
	// command's set/remove/hosts/body flows) are visible to per-agent views
	// on their next use, before and after Save.
	path := writeSecrets(t, `[custom]
key = "v1"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := s.ForAgent("alpha")

	mustMutate(t, s.Set("custom.added", "inproc"))
	if v, ok := view.Get("custom.added"); !ok || v != "inproc" {
		t.Errorf("view Get(custom.added) = %q, %v — want the root Set to be visible", v, ok)
	}

	found, err := s.Remove("custom.key")
	mustMutate(t, err)
	if !found {
		t.Fatal("Remove(custom.key) = false")
	}
	if _, ok := view.Get("custom.key"); ok {
		t.Error("view should see the removal of custom.key")
	}

	mustMutate(t, s.AddAllowedHost("custom", "api.example.com"))
	if hosts := view.SectionAllowedHosts("custom"); len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("view SectionAllowedHosts(custom) = %v — want the root-added host", hosts)
	}

	mustMutate(t, s.AddAllowedInBody("custom", "key2"))
	if !view.IsAllowedInBody("custom.key2") {
		t.Error("view IsAllowedInBody(custom.key2) = false — want the root-added body key")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if v, ok := view.Get("custom.added"); !ok || v != "inproc" {
		t.Errorf("view Get(custom.added) after Save = %q, %v — want inproc", v, ok)
	}

	// A view cannot Save; only the root writes the file.
	if err := view.Save(); err == nil {
		t.Error("view.Save() should fail")
	}
}

func TestReloadRemovedSecretFailsResolve(t *testing.T) {
	// Proves that a secret removed from the file fails later resolution
	// with the existing "unknown secret" error, not with a stale value.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"val\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, "[other]\nkey = \"val\"\n", next())

	_, err = s.Resolve("Bearer {{secret:custom.key}}")
	if err == nil {
		t.Fatal("Resolve should fail for a secret removed from the file")
	}
	if !strings.Contains(err.Error(), `unknown secret: "custom.key"`) {
		t.Errorf("error = %q — want the unknown-secret error", err)
	}
}

func TestReloadRedactsNewValues(t *testing.T) {
	// Proves that Redact uses the current values: a changed value is
	// redacted and the old one no longer is; a newly added value is redacted.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\na = \"aaaa1111\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := s.Redact("x aaaa1111 y"); strings.Contains(got, "aaaa1111") {
		t.Fatalf("setup: original value not redacted: %q", got)
	}

	rewrite(t, path, "[custom]\na = \"bbbb2222\"\nb = \"cccc3333\"\n", next())

	got := s.Redact("x aaaa1111 bbbb2222 cccc3333 y")
	if strings.Contains(got, "bbbb2222") {
		t.Errorf("changed value bbbb2222 not redacted: %q", got)
	}
	if strings.Contains(got, "cccc3333") {
		t.Errorf("new value cccc3333 not redacted: %q", got)
	}
	if !strings.Contains(got, "aaaa1111") {
		t.Errorf("old value aaaa1111 should no longer be redacted: %q", got)
	}
}

func TestReloadRedactsLongestValueFirst(t *testing.T) {
	// Proves the longest-first redaction order holds for reloaded values:
	// when one secret value is a prefix of another, the longer one is
	// replaced whole and no suffix survives a shorter-value-first pass.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nold = \"startup-only\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, "[custom]\nlong = \"shared-prefix-long\"\nshort = \"shared-prefix\"\n", next())

	// Redact draws the values from map iteration, whose order is random
	// per call, so repeat: a pass that replaces the shorter value first
	// leaves "-long" behind and fails.
	for i := 0; i < 50; i++ {
		if got := s.Redact("x shared-prefix-long y"); got != "x [REDACTED] y" {
			t.Fatalf("iteration %d: Redact = %q, want the longer value replaced whole", i, got)
		}
	}
	if got := s.Redact("x shared-prefix y"); got != "x [REDACTED] y" {
		t.Errorf("Redact = %q, want the shorter reloaded value replaced too", got)
	}
}

func TestReloadUpdatesHostsAndBodyRules(t *testing.T) {
	// Proves that allowed_hosts and allowed_in_body changes on disk are
	// enforced by the next read: host permission moves to the new host and
	// body permission moves to the new key.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, `
[custom]
key = "sk-v1"
other = "sk-other"
allowed_hosts = ["old.example.com"]
allowed_in_body = ["key"]
`, next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, `
[custom]
key = "sk-v2"
other = "sk-other"
allowed_hosts = ["new.example.com"]
allowed_in_body = ["other"]
`, next())

	if err := s.CheckHostAllowed("custom.key", "https://new.example.com/x"); err != nil {
		t.Errorf("host check against new allowed host failed: %v", err)
	}
	if err := s.CheckHostAllowed("custom.key", "https://old.example.com/x"); err == nil {
		t.Error("host check against the removed allowed host should fail")
	}
	if s.IsAllowedInBody("custom.key") {
		t.Error("custom.key should no longer be allowed in body")
	}
	if !s.IsAllowedInBody("custom.other") {
		t.Error("custom.other should be allowed in body after the reload")
	}
}

func TestBadFileKeepsLastGoodAndWarnsOnce(t *testing.T) {
	// Proves that an unparseable file keeps the last good contents, logs
	// exactly one warning per bad file state (not one per use), and recovers
	// when the file becomes valid again.
	buf := captureWarns(t)
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"good\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, "this is not valid toml [[[", next())
	for i := 0; i < 3; i++ {
		if v, ok := s.Get("custom.key"); !ok || v != "good" {
			t.Fatalf("use %d: Get(custom.key) = %q, %v — want the last good value", i, v, ok)
		}
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("got %d warnings for the bad state after 3 uses, want exactly 1", n)
	}

	// A different bad state warns once more.
	rewrite(t, path, "still bad ][", next())
	if v, ok := s.Get("custom.key"); !ok || v != "good" {
		t.Fatalf("Get(custom.key) = %q, %v — want the last good value", v, ok)
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 2 {
		t.Errorf("got %d warnings total after a second bad state, want 2", n)
	}

	// Recovery: a valid file loads and no further warning is emitted.
	rewrite(t, path, "[custom]\nkey = \"recovered\"\n", next())
	if v, ok := s.Get("custom.key"); !ok || v != "recovered" {
		t.Errorf("Get(custom.key) = %q, %v — want \"recovered\" after the file became valid", v, ok)
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 2 {
		t.Errorf("warnings grew to %d after recovery, want to stay at 2", n)
	}
}

func TestReloadInvalidSectionKeepsLastGood(t *testing.T) {
	// Proves that a file failing Load's validation (a section with both
	// allowed_agents and denied_agents) keeps the last good contents on
	// reload, warns once, and recovers when fixed.
	buf := captureWarns(t)
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"good\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rewrite(t, path, `
[broken]
token = "val"
allowed_agents = ["alice"]
denied_agents = ["bob"]
`, next())

	if v, ok := s.Get("custom.key"); !ok || v != "good" {
		t.Errorf("Get(custom.key) = %q, %v — want the last good value", v, ok)
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("got %d warnings, want exactly 1", n)
	}

	rewrite(t, path, "[custom]\nkey = \"fixed\"\n", next())
	if v, ok := s.Get("custom.key"); !ok || v != "fixed" {
		t.Errorf("Get(custom.key) = %q, %v — want \"fixed\" after the file became valid", v, ok)
	}
}

func TestDeletedFileKeepsLastGoodAndRecovers(t *testing.T) {
	// Proves that deleting the secrets file keeps the last good contents
	// with exactly one warning for the deleted state, and that a recreated
	// file loads with its new contents.
	buf := captureWarns(t)
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"kept\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if v, ok := s.Get("custom.key"); !ok || v != "kept" {
			t.Fatalf("use %d after delete: Get(custom.key) = %q, %v — want the last good value", i, v, ok)
		}
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("got %d warnings for the deleted state, want exactly 1", n)
	}

	rewrite(t, path, "[custom]\nkey = \"recreated\"\n", next())
	if v, ok := s.Get("custom.key"); !ok || v != "recreated" {
		t.Errorf("Get(custom.key) = %q, %v — want \"recreated\" after the file came back", v, ok)
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("warnings grew to %d after recovery, want to stay at 1", n)
	}
}

func TestMissingSecretsFileCreatedLaterLoads(t *testing.T) {
	// Proves that a secrets file which did not exist at startup is picked
	// up when it is created — no restart needed.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load of missing file: %v", err)
	}
	if names := s.Names(); len(names) != 0 {
		t.Fatalf("setup: Names() = %v, want empty", names)
	}

	rewrite(t, path, "[custom]\nkey = \"late\"\n", mtimeSeq()())

	if v, ok := s.Get("custom.key"); !ok || v != "late" {
		t.Errorf("Get(custom.key) = %q, %v — want \"late\" after the file was created", v, ok)
	}
	if names := s.Names(); len(names) != 1 || names[0] != "custom.key" {
		t.Errorf("Names() = %v — want [custom.key]", names)
	}
}

func TestBlockedPathsSurviveReload(t *testing.T) {
	// Proves that the blocked-path policy is store lifetime state, not file
	// content: after a reload the secrets file path, the defaults, and paths
	// added at runtime are all still blocked, for the root and views alike.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[custom]\nkey = \"v1\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := s.ForAgent("alpha")
	s.AddBlockedPaths([]string{".env"})

	rewrite(t, path, "[custom]\nkey = \"v2\"\n", next())
	if v, ok := s.Get("custom.key"); !ok || v != "v2" {
		t.Fatalf("setup: reload not observed — Get(custom.key) = %q, %v, want v2", v, ok)
	}

	for name, store := range map[string]*Store{"root": s, "view": view} {
		if !store.IsBlockedPath(path) {
			t.Errorf("%s: the secrets file path should stay blocked after a reload", name)
		}
		if !store.IsBlockedPath("secrets.toml") || !store.IsBlockedPath("/proc/self/environ") {
			t.Errorf("%s: default blocked paths should stay blocked after a reload", name)
		}
		if !store.IsBlockedPath(".env") {
			t.Errorf("%s: runtime-added blocked path should stay blocked after a reload", name)
		}
		if !store.IsBlockedCommand("cat " + path) {
			t.Errorf("%s: the secrets file path should stay blocked in commands", name)
		}
	}
}

func TestConcurrentReloadMutateAndRead(t *testing.T) {
	// Proves the store is safe under concurrent use: while reader goroutines
	// hammer Resolve/Redact/Get/Names through a per-agent view and the root,
	// the root is mutated in-process and the file is rewritten (triggering
	// reloads). Must pass under -race; the final read must observe the last
	// file state. The file rewrite comes last, so the file (the source of
	// truth) wins over the earlier in-process mutations.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, `
[custom]
api_key = "sk-v1"
payload = "redact-me-v1"
`, next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := s.ForAgent("alpha")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				view.Resolve("Bearer {{secret:custom.api_key}}")
				view.Redact("x redact-me-v1 y")
				view.Names()
				s.Get("custom.api_key")
			}
		}()
	}

	for n := 0; n < 25; n++ {
		mustMutate(t, s.Set("custom.api_key", fmt.Sprintf("mut-%d", n)))
		s.Get("custom.api_key")
	}
	rewrite(t, path, `
[custom]
api_key = "sk-file-final"
payload = "redact-me-final"
`, next())
	if v, ok := s.Get("custom.api_key"); !ok || v != "sk-file-final" {
		t.Fatalf("post-rewrite Get(custom.api_key) = %q, %v — want sk-file-final", v, ok)
	}

	close(stop)
	wg.Wait()

	if v, ok := s.Get("custom.api_key"); !ok || v != "sk-file-final" {
		t.Errorf("final Get(custom.api_key) = %q, %v — want the file's sk-file-final (file is the source of truth)", v, ok)
	}
	if resolved, err := view.Resolve("Bearer {{secret:custom.api_key}}"); err != nil || resolved != "Bearer sk-file-final" {
		t.Errorf("final view Resolve = %q, %v — want the file's value", resolved, err)
	}
}

func TestSaveRacingReadsKeepsEverySecret(t *testing.T) {
	// Proves the Save-vs-readers race is closed: while reader goroutines
	// hammer the store, repeated root Set+Save and external non-atomic
	// rewrites can neither expose a non-complete state to readers (Save
	// holds the write lock across its write, so a Resolve error during the
	// in-process saves is impossible) nor lose a secret from memory or from
	// the file. The final external rewrite must also be picked up — on code
	// without reload that assertion fails.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, `
[anthropic]
api_key = "sk-precious"

[svc]
key = "v0"
`, next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := s.ForAgent("alpha")

	var resolveErrs atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := view.Resolve("Bearer {{secret:svc.key}}"); err != nil {
					resolveErrs.Add(1)
				}
				view.Redact("x sk-precious y")
				s.Names()
			}
		}()
	}

	// Phase 1: in-process saves race the readers.
	for n := 0; n < 100; n++ {
		mustMutate(t, s.Set("svc.key", fmt.Sprintf("v%d", n)))
		if err := s.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	if n := resolveErrs.Load(); n != 0 {
		t.Fatalf("%d Resolve errors while Save raced the readers — a reader observed a non-complete state", n)
	}
	if v, ok := s.Get("anthropic.api_key"); !ok || v != "sk-precious" {
		t.Fatalf("after racing saves: Get(anthropic.api_key) = %q, %v — a secret was lost from memory", v, ok)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "sk-precious") {
		t.Fatalf("racing saves lost anthropic.api_key from secrets.toml:\n%s", disk)
	}

	// Phase 2: external non-atomic rewrites (an editor, the CLI) race the
	// readers. Torn reads from another writer are transient by design; the
	// final state must still be picked up with nothing lost for good.
	for n := 0; n < 50; n++ {
		content := fmt.Sprintf("[anthropic]\napi_key = \"sk-precious\"\n\n[svc]\nkey = \"x%d\"\n", n)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(t, path, "[anthropic]\napi_key = \"sk-precious\"\n\n[svc]\nkey = \"final\"\n", next())

	close(stop)
	wg.Wait()

	if v, ok := s.Get("anthropic.api_key"); !ok || v != "sk-precious" {
		t.Errorf("final Get(anthropic.api_key) = %q, %v — want sk-precious", v, ok)
	}
	if resolved, err := view.Resolve("Bearer {{secret:svc.key}}"); err != nil || resolved != "Bearer final" {
		t.Errorf("final view Resolve = %q, %v — want the file's \"final\" value", resolved, err)
	}
}

func TestReloadStampsThePreReadObservation(t *testing.T) {
	// Pins the anti-torn-read invariant of reloadLocked: the observation
	// stamped as loaded is the one taken BEFORE reading the file, never a
	// fresh one taken after. A non-atomic writer can finish between the
	// read and a post-read stat; stamping that later observation would pin
	// a possibly-truncated parse as the file's final state — it is then
	// never re-read, and the next Save writes the truncation back to disk,
	// deleting secrets. Stamping the pre-read observation is conservative:
	// the finished write differs from it and forces a re-parse. The
	// interleave needs a real race to observe black-box, so the invariant
	// is asserted directly.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[svc]\nkey = \"v1\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Reproduce the interleave deterministically: take the observation of
	// the OLD file, let the writer finish, then reload holding the earlier
	// observation — exactly what reloadLocked cannot control internally.
	s.src.mu.Lock()
	cur := statFile(path)
	rewrite(t, path, "[svc]\nkey = \"v2\"\n", next())
	s.src.reloadLocked(cur)
	loaded := s.src.loaded
	s.src.mu.Unlock()

	if !loaded.same(cur) {
		t.Fatal("reloadLocked stamped an observation other than the one it was decided from — a torn read could be pinned as the file's final state")
	}

	// The next ordinary read re-parses rather than trusting that reload.
	if v, ok := s.Get("svc.key"); !ok || v != "v2" {
		t.Errorf("Get(svc.key) = %q, %v — want v2 after re-parsing the newer file", v, ok)
	}
}

func TestMutatorMergesUnseenFileEdit(t *testing.T) {
	// Proves mutators re-read the file before applying (no lost update):
	// a hand edit the store has not yet observed survives the next
	// in-process Set+Save instead of being silently overwritten.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[svc]\nhand = \"h1\"\n", next())
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// An edit nobody has observed — no store read since it landed.
	rewrite(t, path, "[svc]\nhand = \"h1\"\nchat2 = \"c2\"\n", next())

	mustMutate(t, s.Set("svc.chat", "v1"))
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"h1", "c2", "v1"} {
		if !strings.Contains(string(disk), want) {
			t.Errorf("secrets.toml after Set+Save lacks %q — the unseen file edit was dropped:\n%s", want, disk)
		}
	}
	if v, ok := s.Get("svc.chat2"); !ok || v != "c2" {
		t.Errorf("Get(svc.chat2) = %q, %v — want the merged hand edit", v, ok)
	}
}

func TestSeparateStoreSaveIsSeenByExistingView(t *testing.T) {
	// Proves the reload also covers a writer in another process: a second
	// store loaded from the same file (what `foci secrets set` is) Set+Save's,
	// and a view the first store handed out BEFORE that resolves the new
	// value on its next use (#1269).
	path := writeSecrets(t, "[svc]\nkey = \"aaaa1111\"\nallowed_hosts = [\"h\"]\n")
	gw, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := gw.ForAgent("alice")

	cli, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	mustMutate(t, cli.Set("svc.new", "cli12345"))
	if err := cli.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if r, err := view.Resolve("{{secret:svc.new}}"); err != nil || r != "cli12345" {
		t.Errorf("view Resolve = %q, %v — want the value the second store saved", r, err)
	}
}

func TestMutateOverBrokenFileKeepsLastGood(t *testing.T) {
	// Proves what an in-process mutation does when the file on disk has
	// become unparseable: the mutator's refresh fails with exactly one
	// warning, the mutation applies to the last good contents, and Save
	// writes those merged contents — the unparseable fragment is dropped,
	// because a fragment that cannot be parsed cannot be merged.
	buf := captureWarns(t)
	path := writeSecrets(t, "[svc]\nkey = \"aaaa1111\"\n")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	next := mtimeSeq()
	rewrite(t, path, "[svc]\nkey = \"aaaa1111\"\nhand = \"half-typed\n", next()) // unbalanced quote

	mustMutate(t, s.Set("svc.chat", "chat1234"))
	if err := s.Save(); err != nil {
		t.Fatalf("Save over a broken file: %v", err)
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("%d warnings for the broken file — want exactly 1", n)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`key = "aaaa1111"`, `chat = "chat1234"`} {
		if !strings.Contains(string(disk), want) {
			t.Errorf("secrets.toml after Set+Save lacks %q:\n%s", want, disk)
		}
	}
	if strings.Contains(string(disk), "half-typed") {
		t.Errorf("secrets.toml after Set+Save kept the unparseable fragment:\n%s", disk)
	}
}

func TestFailedSaveKeepsMutationLiveForViews(t *testing.T) {
	// Proves store-first semantics when the file cannot be written: after a
	// failed Save the mutation is still served to the agents (reads come
	// from the store, which remains authoritative), while the disk keeps
	// its old contents.
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod cannot make the file unwritable")
	}
	path := writeSecrets(t, "[svc]\nkey = \"aaaa1111\"\n")
	root, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := root.ForAgent("alice")

	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })

	mustMutate(t, root.Set("svc.key", "unsaved99"))
	if err := root.Save(); err == nil {
		t.Fatal("Save on a read-only file unexpectedly succeeded")
	}
	if v, ok := view.Get("svc.key"); !ok || v != "unsaved99" {
		t.Errorf("view Get = %q, %v — want the unsaved mutation", v, ok)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), "unsaved99") || !strings.Contains(string(disk), "aaaa1111") {
		t.Errorf("disk = %q — want the pre-Save contents only", disk)
	}
}

func TestUnreadableFileKeepsLastGoodAndWarnsOnce(t *testing.T) {
	// Proves the keep-last-good arm for a file that stats but cannot be
	// read (permissions): reads keep answering the last good contents with
	// exactly one warning for that file state, and the store recovers only
	// when a stamp changes again — fixing the permissions alone changes
	// neither mtime, size nor identity, so it does not trigger a re-read.
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod cannot make the file unreadable")
	}
	buf := captureWarns(t)
	path := writeSecrets(t, "[svc]\nkey = \"aaaa1111\"\n")
	root, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := root.ForAgent("alice")

	next := mtimeSeq()
	rewrite(t, path, "[svc]\nkey = \"bbbb2222\"\n", next())
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })

	for i := 0; i < 50; i++ {
		if v, ok := view.Get("svc.key"); !ok || v != "aaaa1111" {
			t.Fatalf("Get under an unreadable file = %q, %v — want the last good value", v, ok)
		}
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Fatalf("%d warnings for the unreadable file — want exactly 1", n)
	}

	// Permissions restored, stamp untouched: still the last good contents.
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if v, ok := view.Get("svc.key"); !ok || v != "aaaa1111" {
		t.Fatalf("Get after a bare chmod back = %q, %v — no stamp changed, so no re-read was due", v, ok)
	}

	// A stamp change on the readable file is picked up.
	rewrite(t, path, "[svc]\nkey = \"bbbb2222\"\n", next())
	if v, ok := view.Get("svc.key"); !ok || v != "bbbb2222" {
		t.Errorf("Get after the file changed again = %q, %v — want the reloaded value", v, ok)
	}
}

func TestDirectoryAtPathKeepsLastGoodAndStaysBlocked(t *testing.T) {
	// Proves the keep-last-good arm for a directory replacing the file
	// (the stat succeeds, the read fails): the last good contents keep
	// being served, exactly one warning is logged, and the secrets path
	// stays blocked for exec after the failed reloads.
	buf := captureWarns(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	next := mtimeSeq()
	rewrite(t, path, "[svc]\nkey = \"aaaa1111\"\n", next())
	root, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := root.ForAgent("alice")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		if v, ok := view.Get("svc.key"); !ok || v != "aaaa1111" {
			t.Fatalf("Get with a directory at the path = %q, %v — want the last good value", v, ok)
		}
	}
	if n := strings.Count(buf.String(), reloadWarnMarker); n != 1 {
		t.Errorf("%d warnings for the directory — want exactly 1", n)
	}
	if !view.IsBlockedPath(path) || !view.IsBlockedCommand("cat " + path) {
		t.Errorf("the secrets path stopped being blocked after failed reloads")
	}
}
