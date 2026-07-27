// Package sessionenv owns the per-session exec-bridge environment mapping
// that shared-subprocess backends use to route bash spawns to the right
// session.
//
// PROBLEM (shared by opencode and codex): a backend that runs ONE subprocess
// per agent, shared across all of that agent's sessions, has ONE environment —
// captured from whichever session started it and then frozen. FOCI_SOCK,
// BASH_ENV and FOCI_SESSION_KEY are set PER SESSION by DelegatedManager, so
// every session after the first inherits the first session's exec bridge:
// session-scoped tools (foci_ask, send_to_session, foci_send_to_chat) route to
// the wrong chat. Silently — nothing errors.
//
// FIX (shape shared by both backends): foci writes a small JSON file per
// session/thread into {tempdir}/session-env/, and an injector running inside
// the backend reads it immediately before each bash spawn and overrides the
// process defaults. The injector differs per backend — opencode uses a
// generated `shell.env` plugin, codex a PreToolUse hook that rewrites the
// command — but the file format, its location, its lifecycle and the
// idempotent artifact writer that installs the injector are identical, and
// live here.
//
// The file is keyed by the ID the injector sees at spawn time: opencode's
// session ID, codex's thread ID.
package sessionenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"foci/internal/tempdir"
)

// Subdir is the tempdir subdirectory holding the per-session env files.
const Subdir = "session-env"

// Entry is the JSON wire format both injectors read. Only the bridge vars
// that differ per session are carried; everything else inherits from the
// shared subprocess environment.
//
// The JSON field names ARE the environment variable names — the opencode
// plugin Object.assign's the decoded object straight over the spawn env, so
// renaming a field renames the variable.
type Entry struct {
	FociSock   string `json:"FOCI_SOCK,omitempty"`
	BashEnv    string `json:"BASH_ENV,omitempty"`
	SessionKey string `json:"FOCI_SESSION_KEY,omitempty"`
}

// Var is one name/value pair from an Entry, in a stable order.
type Var struct {
	Name  string
	Value string
}

// EntryFrom extracts the per-session bridge vars from a full env map,
// ignoring everything else.
func EntryFrom(env map[string]string) Entry {
	return Entry{
		FociSock:   env["FOCI_SOCK"],
		BashEnv:    env["BASH_ENV"],
		SessionKey: env["FOCI_SESSION_KEY"],
	}
}

// IsZero reports whether the entry carries nothing worth writing.
func (e Entry) IsZero() bool {
	return e.FociSock == "" && e.BashEnv == "" && e.SessionKey == ""
}

// Vars returns the entry's non-empty variables in a fixed order. Callers that
// render the entry into a shell prefix depend on the order being stable, so
// the rendered command (and therefore any hash of it) is reproducible.
func (e Entry) Vars() []Var {
	out := make([]Var, 0, 3)
	for _, v := range []Var{
		{"FOCI_SESSION_KEY", e.SessionKey},
		{"FOCI_SOCK", e.FociSock},
		{"BASH_ENV", e.BashEnv},
	} {
		if v.Value != "" {
			out = append(out, v)
		}
	}
	return out
}

// Dir returns the directory holding the per-session env files. Callers that
// need to hand it to another process (the codex hook's --env-dir) must read it
// here rather than reconstructing it, so the writer and the reader can never
// disagree about where the bindings live.
func Dir() string {
	return filepath.Join(tempdir.Dir(), Subdir)
}

// Path returns the env-file path for one session/thread ID.
func Path(id string) string {
	return filepath.Join(Dir(), id+".json")
}

// Write records the per-session bridge env for id. A zero entry (no bridge
// vars in env) writes nothing and is not an error: the injector then leaves
// the spawn env untouched, which is exactly the pre-existing behaviour.
func Write(id string, env map[string]string) error {
	if id == "" {
		return nil
	}
	entry := EntryFrom(env)
	if entry.IsZero() {
		return nil
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session-env mkdir %s: %w", dir, err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session-env marshal %s: %w", id, err)
	}
	if err := os.WriteFile(Path(id), data, 0o644); err != nil {
		return fmt.Errorf("session-env write %s: %w", id, err)
	}
	return nil
}

// Remove deletes the per-session env file. A missing file is not an error.
func Remove(id string) error {
	if id == "" {
		return nil
	}
	if err := os.Remove(Path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session-env remove %s: %w", id, err)
	}
	return nil
}

// Load reads the entry for id — the reader side of Write, used by the codex
// hook binary. The directory is a parameter rather than resolved from tempdir
// because that reader is a grandchild of foci-gw (via codex) and an inherited
// FOCI_TMPDIR is not something to bet a silent misroute on: foci tells it
// exactly which directory it wrote to.
//
// A missing file yields a zero Entry and a nil error: "no per-session
// override" is the normal case, not a failure.
func Load(dir, id string) (Entry, error) {
	var e Entry
	if dir == "" || id == "" {
		return e, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return e, nil
		}
		return e, fmt.Errorf("session-env read %s: %w", id, err)
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("session-env parse %s: %w", id, err)
	}
	return e, nil
}

// EnsureFile writes a foci-generated artifact (an opencode plugin, the codex
// hook's marker files) to path if it is missing or its content has gone
// stale, creating parent directories as needed. Reports whether it wrote.
//
// Idempotent by content: an unchanged artifact is not rewritten, so mtime is
// left alone and file-watchers (opencode reloads plugins on change) don't fire
// on every session start.
func EnsureFile(path, content string, perm os.FileMode) (bool, error) {
	if path == "" {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("ensure %s: mkdir: %w", path, err)
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return false, fmt.Errorf("ensure %s: write: %w", path, err)
	}
	return true, nil
}
