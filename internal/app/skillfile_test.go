package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/fap"
	"foci/internal/skills"
	"foci/internal/turnevent"
)

// skillFixture is a real skill tree plus a hub whose agent "ag" has it as its
// skill scope, and a paired device's token (#2392):
//
//	root/prediction/{SKILL.md,method.md,notes.txt}
//	root/prediction/linkfile.md -> outside/secret.md   (symlinked file, target outside the roots)
//	root/linked -> outside/linked                      (symlinked skill dir, target outside the roots)
//	elsewhere/x/SKILL.md                               (under no root)
type skillFixture struct {
	h                        *Hub
	tok                      string
	root, outside, elsewhere string
}

const skillMDBody = "---\nname: prediction\ndescription: \"forecasts: record and score\"\n---\n# Prediction\n\nbody v1\n"

func newSkillFixture(t *testing.T) skillFixture {
	t.Helper()
	base := t.TempDir()
	f := skillFixture{root: filepath.Join(base, "root"), outside: filepath.Join(base, "outside"), elsewhere: filepath.Join(base, "elsewhere")}
	for path, body := range map[string]string{
		filepath.Join(f.root, "prediction", "SKILL.md"):  skillMDBody,
		filepath.Join(f.root, "prediction", "method.md"): "method v1\n",
		filepath.Join(f.root, "prediction", "notes.txt"): "not markdown\n",
		filepath.Join(f.outside, "secret.md"):            "outside file\n",
		filepath.Join(f.outside, "linked", "SKILL.md"):   "---\nname: linked\n---\n",
		filepath.Join(f.outside, "linked", "extra.md"):   "linked extra\n",
		filepath.Join(f.elsewhere, "x", "SKILL.md"):      "---\nname: x\n---\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(f.outside, "linked"), filepath.Join(f.root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.outside, "secret.md"), filepath.Join(f.root, "prediction", "linkfile.md")); err != nil {
		t.Fatal(err)
	}
	f.h = newTestHub()
	f.tok = f.h.devices.pair("phone", "").Token
	registerFakeAgent(f.h, "ag").scope = skills.ReadScope{WorkDir: base, Dirs: skills.Dirs{Shared: f.root}}
	return f
}

// get issues GET target (a "/app/skill/..." URL) with the given bearer token
// ("" = none) straight into ServeSkillFile.
func (f skillFixture) get(t *testing.T, target, tok string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	f.h.ServeSkillFile(w, req)
	return w
}

func skillTarget(agentID, path string) string {
	return "/app/skill/" + agentID + "?path=" + url.QueryEscape(path)
}

// TestServeSkillFile_ServesCurrentFile: a file under a root is served whole —
// frontmatter included, unparsed — and an edit after the first fetch shows on
// the next one: the current file, not a snapshot.
func TestServeSkillFile_ServesCurrentFile(t *testing.T) {
	f := newSkillFixture(t)
	path := filepath.Join(f.root, "prediction", "SKILL.md")

	w := f.get(t, skillTarget("ag", path), f.tok)
	if w.Code != http.StatusOK || w.Body.String() != skillMDBody {
		t.Fatalf("first fetch = %d %q, want 200 with the whole file", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content-type = %q, want text/markdown; charset=utf-8", ct)
	}

	edited := skillMDBody + "\nbody v2 — edited after the chip was sent\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := f.get(t, skillTarget("ag", path), f.tok); w.Code != http.StatusOK || w.Body.String() != edited {
		t.Fatalf("after edit = %d %q, want 200 with the edited file", w.Code, w.Body.String())
	}
}

// TestServeSkillFile_FollowsSymlinks: a symlinked skill dir and a symlinked
// file under a root are served even though their targets are outside every
// root (Dick 2026-10-13: "Some skills use symlinks! They should be followed").
func TestServeSkillFile_FollowsSymlinks(t *testing.T) {
	f := newSkillFixture(t)
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(f.root, "linked", "extra.md"), "linked extra\n"},
		{filepath.Join(f.root, "prediction", "linkfile.md"), "outside file\n"},
	} {
		if w := f.get(t, skillTarget("ag", tc.path), f.tok); w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tc.path, w.Code, w.Body.String(), tc.want)
		}
	}
}

// TestServeSkillFile_Refusals: every path outside the rule is a 404 with the
// same body whether or not a file exists there; a missing servable file is a
// 410; a file the server cannot read is a 500; and without valid device auth
// nothing is served.
func TestServeSkillFile_Refusals(t *testing.T) {
	f := newSkillFixture(t)
	refused := []struct{ name, target string }{
		{"dot-dot escape", "/app/skill/ag?path=" + url.QueryEscape(f.root+"/x/../../etc/passwd.md")},
		{"dot-dot escape to an existing file", "/app/skill/ag?path=" + url.QueryEscape(f.root+"/prediction/../../outside/secret.md")},
		{"non-md file under a root", skillTarget("ag", filepath.Join(f.root, "prediction", "notes.txt"))},
		{"absolute path under no root (exists)", skillTarget("ag", filepath.Join(f.elsewhere, "x", "SKILL.md"))},
		{"absolute path under no root (missing)", skillTarget("ag", filepath.Join(f.elsewhere, "nope", "SKILL.md"))},
		{"resolved target of a symlink, asked for directly", skillTarget("ag", filepath.Join(f.outside, "secret.md"))},
		{"relative path", skillTarget("ag", "prediction/SKILL.md")},
		{"no path", "/app/skill/ag"},
		{"unknown agent", skillTarget("ghost", filepath.Join(f.root, "prediction", "SKILL.md"))},
		{"no agent", skillTarget("", filepath.Join(f.root, "prediction", "SKILL.md"))},
	}
	bodies := map[string]bool{}
	for _, tc := range refused {
		w := f.get(t, tc.target, f.tok)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d, want 404", tc.name, w.Code)
		}
		bodies[w.Body.String()] = true
	}
	if len(bodies) != 1 {
		t.Errorf("refusals must read the same; got bodies %v", bodies)
	}

	if w := f.get(t, skillTarget("ag", filepath.Join(f.root, "prediction", "gone.md")), f.tok); w.Code != http.StatusGone {
		t.Errorf("missing servable file: code = %d, want 410", w.Code)
	}

	if os.Geteuid() != 0 { // root reads a 0o000 file, so the 500 arm needs a normal user
		locked := filepath.Join(f.root, "prediction", "locked.md")
		if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		if w := f.get(t, skillTarget("ag", locked), f.tok); w.Code != http.StatusInternalServerError {
			t.Errorf("unreadable file: code = %d, want 500", w.Code)
		}
	}

	good := skillTarget("ag", filepath.Join(f.root, "prediction", "SKILL.md"))
	if w := f.get(t, good, ""); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "Prediction") {
		t.Errorf("no token: code = %d body %q, want 401 and no content", w.Code, w.Body.String())
	}
	if w := f.get(t, good, "not-a-real-token"); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "Prediction") {
		t.Errorf("bad token: code = %d body %q, want 403 and no content", w.Code, w.Body.String())
	}
}

// rawFrames returns every queued wire frame on c, undecoded.
func rawFrames(c *wsClient) [][]byte {
	var out [][]byte
	for {
		select {
		case b := <-c.send:
			out = append(out, b)
		default:
			return out
		}
	}
}

// TestAppSink_SkillChipCarriesFileURL: with the agent's skill scope, a read
// of a SKILL.md or a sub-file under a root sends a chip whose SkillURL fetches
// that file's current text; a SKILL.md under no root keeps today's text-only
// chip; the same file read twice is two chips; a sub-agent's activity sends
// none. The chip is the existing notification frame, so an app that predates
// SkillURL decodes it to the same text row (#2392).
func TestAppSink_SkillChipCarriesFileURL(t *testing.T) {
	f := newSkillFixture(t)
	c := fakeClient()
	b := &convBinding{convID: "c1", agentID: "ag", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.skillScopeFn = f.h.PrimaryBot("ag").agentRef.SkillScope
	ctx := context.Background()
	read := func(path string) {
		s.Emit(ctx, turnevent.ToolCall{Name: "Read", Args: json.RawMessage(`{"file_path":` + strconvQuote(path) + `}`)})
	}

	skillMD := filepath.Join(f.root, "prediction", "SKILL.md")
	method := filepath.Join(f.root, "prediction", "method.md")
	read(skillMD)
	read(method)
	read(method) // same file twice in the turn
	read(filepath.Join(f.elsewhere, "x", "SKILL.md"))
	s.Emit(ctx, turnevent.SubagentStart{GroupKey: "g", Label: "read " + skillMD, Prompt: "Read " + method})
	s.Emit(ctx, turnevent.SubagentText{GroupKey: "g", Text: "I read " + skillMD})
	s.Emit(ctx, turnevent.SubagentEnd{GroupKey: "g"})

	// oldNotification is the notification as an app built before #2392 knows
	// it: no skillUrl. Unknown keys are ignored (fap.go), as the Kotlin
	// decoder ignores them.
	type oldNotification struct {
		ConversationID string `json:"conversationId"`
		MessageID      string `json:"messageId"`
		Text           string `json:"text"`
		Level          string `json:"level"`
	}
	type chip struct{ text, url, id string }
	var chips []chip
	for _, frame := range rawFrames(c) {
		var env struct {
			T string          `json:"t"`
			D json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(frame, &env); err != nil {
			t.Fatalf("bad wire frame: %v", err)
		}
		if env.T != fap.TypeNotification {
			continue
		}
		var old oldNotification
		if err := json.Unmarshal(env.D, &old); err != nil {
			t.Fatalf("old-app decode: %v", err)
		}
		var now fap.Notification
		if err := json.Unmarshal(env.D, &now); err != nil {
			t.Fatal(err)
		}
		if old.Text != now.Text || old.Text == "" {
			t.Errorf("old-app text %q, want the chip text %q", old.Text, now.Text)
		}
		chips = append(chips, chip{old.Text, now.SkillURL, old.MessageID})
	}

	want := []chip{
		{"📖 Loaded skill: prediction", skillTarget("ag", skillMD), ""},
		{"📖 Loaded skill file: prediction/method.md", skillTarget("ag", method), ""},
		{"📖 Loaded skill file: prediction/method.md", skillTarget("ag", method), ""},
		{"📖 Loaded skill: x", "", ""},
	}
	if len(chips) != len(want) {
		t.Fatalf("chips = %+v, want %d (none from the sub-agent)", chips, len(want))
	}
	for i := range want {
		if chips[i].text != want[i].text || chips[i].url != want[i].url {
			t.Errorf("chip %d = (%q, %q), want (%q, %q)", i, chips[i].text, chips[i].url, want[i].text, want[i].url)
		}
	}
	if chips[1].id == chips[2].id {
		t.Errorf("the two reads of the same file share message id %q: want two chips", chips[1].id)
	}

	// The chip's URL is what the app fetches: it returns the file as it is now.
	if err := os.WriteFile(method, []byte("method v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := f.get(t, chips[1].url, f.tok); w.Code != http.StatusOK || w.Body.String() != "method v2\n" {
		t.Errorf("fetching the chip's SkillURL = %d %q, want 200 \"method v2\\n\"", w.Code, w.Body.String())
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
