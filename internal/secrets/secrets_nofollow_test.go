package secrets

// secrets_nofollow_test.go: LoadNoFollow — the read-through-one-verified-
// descriptor loader for privileged pre-flight checks (foci check config runs as
// root while the config directory stays writable by the service user). The
// bytes a caller gets are the bytes the open(2) O_NOFOLLOW descriptor
// yielded: a symlink fails the open, a non-regular file is refused, and the
// store serves the verified bytes for its lifetime even if the path changes
// underneath it afterwards.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"foci/internal/log"
)

// TestLoadNoFollow_ParsesLikeLoad proves the no-follow loader parses with
// Load's own rules: the same file through Load and LoadNoFollow answers the
// same keys, for the root store and a per-agent view.
func TestLoadNoFollow_ParsesLikeLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	body := `
[slack]
bot_token = "xoxb-v"

[agents.helen.slack]
bot_token = "xoxb-agent"
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	noFollow, err := LoadNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*Store{loaded, noFollow} {
		if v, ok := store.Get("slack.bot_token"); !ok || v != "xoxb-v" {
			t.Errorf("Get(slack.bot_token) = %q, %v; want the parsed value", v, ok)
		}
		if v, ok := store.ForAgent("helen").Get("slack.bot_token"); !ok || v != "xoxb-agent" {
			t.Errorf("per-agent Get(slack.bot_token) = %q, %v; want the agent override", v, ok)
		}
	}
}

// TestLoadNoFollow_RefusesSymlink proves a final symlink component fails the
// open (ELOOP) instead of being followed: the root reader never sees the
// target's bytes, whatever the target holds.
func TestLoadNoFollow_RefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.toml")
	if err := os.WriteFile(target, []byte("[slack]\nbot_token = \"xoxb-leaked\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "secrets.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoFollow(link)
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("LoadNoFollow(symlink) err = %v, want ELOOP", err)
	}
}

// TestLoadNoFollow_RefusesSymlinkedDirectoryComponent proves the walk
// refuses a symlinked DIRECTORY component too: O_NOFOLLOW on the final
// component alone would let a link planted one level up redirect the root
// reader to any secrets.toml it can open, so every component is opened with
// openat beneath the last and a symlink at any depth fails with ELOOP.
func TestLoadNoFollow_RefusesSymlinkedDirectoryComponent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real-config")
	if err := os.MkdirAll(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "secrets.toml"), []byte("[slack]\nbot_token = \"xoxb-leaked\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "config")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoFollow(filepath.Join(alias, "secrets.toml"))
	// ELOOP or ENOTDIR depending on the kernel's check order for
	// O_NOFOLLOW|O_DIRECTORY; both mean the symlink was not traversed.
	if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("LoadNoFollow(symlinked directory) err = %v, want ELOOP or ENOTDIR (not traversed)", err)
	}
	if !strings.Contains(err.Error(), `"config"`) {
		t.Errorf("err = %v, want the refused component named", err)
	}
}

// TestLoadNoFollow_ParseErrorNoValueEcho proves a malformed file's parse
// error never echoes its bytes: the TOML parser's message quotes the
// fragment it choked on — a secret's prefix is still a fragment of the
// secret — so the error must be exactly the ParseError text and nothing
// else; only the line number leaves the package.
func TestLoadNoFollow_ParseErrorNoValueEcho(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(path, []byte("[slack]\nbot_token = xoxb-unquoted-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoFollow(path)
	if err == nil {
		t.Fatal("malformed secrets loaded; want the parse refusal")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("parse error = %T (%v), want *ParseError", err, err)
	}
	// Exact equality, not a substring ban: a wrapped parser error leaks the
	// fragment it choked on ("xoxb" alone is a token prefix), and any
	// extra byte in this message is file content.
	if got, want := err.Error(), "parse secrets: malformed at line 2"; got != want {
		t.Errorf("parse error = %q, want exactly %q (only the line number leaves the package)", got, want)
	}
}

// TestLoadNoFollow_RefusesNonRegular proves a path that is not a regular
// file (a directory; the same check refuses a fifo before any read can
// block) is refused after the open, on the descriptor's own kind.
func TestLoadNoFollow_RefusesNonRegular(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoFollow(path)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("LoadNoFollow(directory) err = %v, want the not-a-regular-file refusal", err)
	}
}

// TestLoadNoFollow_MissingFileIsError proves a missing file is an error
// (Load's empty-store special case belongs to Load; the caller of
// LoadNoFollow asked for the bytes behind one specific path).
func TestLoadNoFollow_MissingFileIsError(t *testing.T) {
	t.Parallel()
	if _, err := LoadNoFollow(filepath.Join(t.TempDir(), "absent.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadNoFollow(missing) err = %v, want ErrNotExist", err)
	}
}

// TestLoadNoFollow_StaticSnapshot proves the returned store serves the
// verified bytes for its lifetime — the file changing or disappearing
// afterwards changes nothing — and refuses writes, which have no honest
// meaning for a snapshot with no path.
func TestLoadNoFollow_StaticSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(path, []byte("[slack]\nbot_token = \"xoxb-v\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}

	// The path mutates and then disappears: the snapshot keeps answering.
	if err := os.WriteFile(path, []byte("[slack]\nbot_token = \"xoxb-replaced\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, _ := store.Get("slack.bot_token"); v != "xoxb-v" {
		t.Errorf("Get after rewrite = %q; a static snapshot serves the verified bytes", v)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if v, ok := store.Get("slack.bot_token"); !ok || v != "xoxb-v" {
		t.Errorf("Get after removal = %q, %v; a static snapshot outlives its path", v, ok)
	}

	if err := store.Set("slack.bot_token", "xoxb-x"); err == nil || !strings.Contains(err.Error(), "static") {
		t.Errorf("Set on a static store = %v, want the static refusal", err)
	}
	if _, err := store.Remove("slack.bot_token"); err == nil || !strings.Contains(err.Error(), "static") {
		t.Errorf("Remove on a static store = %v, want the static refusal", err)
	}
	if err := store.Save(); err == nil || !strings.Contains(err.Error(), "static") {
		t.Errorf("Save on a static store = %v, want the static refusal", err)
	}
}

// TestLoadNoFollow_ReadsNeverTouchTheReloadPath proves reads from a static
// snapshot stay off the file-reload machinery entirely: the store has no path
// to stat and nothing to re-read, so its first read must not escalate into a
// reload attempt. Observed through a log tap (#2357) as the absence of the
// reload warning a read-driven reload would emit — the warning is the
// empty-path form ("secrets file  changed", a double space), which only a
// pathless static source can format, so parallel reload tests with real
// paths cannot trip it.
func TestLoadNoFollow_ReadsNeverTouchTheReloadPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(path, []byte("[slack]\nbot_token = \"xoxb-v\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}

	var warned atomic.Bool
	remove := log.AddTap(log.WARN, func(level log.Level, component, msg string) {
		if component == "secrets" && strings.Contains(msg, "secrets file  changed but could not be reloaded") {
			warned.Store(true)
		}
	})
	defer remove()

	if v, ok := store.Get("slack.bot_token"); !ok || v != "xoxb-v" {
		t.Fatalf("Get = %q, %v; the snapshot must serve its verified bytes", v, ok)
	}
	if warned.Load() {
		t.Error("reading a static snapshot warned about reloading; a LoadNoFollow store has no path to stat or re-read")
	}
}
