// blank_system.go — Replace opencode's built-in default prompts with foci's,
// via a seeded plugin. Two hooks, both reading a per-session file foci writes
// at Start:
//   - experimental.chat.system.transform → replaces opencode's default system
//     prompt ("You are opencode…") with foci's per-session character prompt.
//   - experimental.session.compacting → replaces opencode's built-in
//     compaction template with foci's compaction-summary.md, so opencode's
//     internal /summarize follows foci's format (same prompt the CC backend
//     uses). Sets output.prompt.
//
// A named agent with a minimal (" ") prompt does NOT suppress the system
// default — verified against opencode 1.17.15 by capturing the outgoing
// provider request (and requesting an unregistered "foci" agent jammed every
// opencode agent, 2026-07-08). The transform hooks are the working lever.
//
// Why a per-session FILE (not baked into the plugin, not a live foci RPC):
// one `opencode serve` is shared across ALL of an agent's sessions and loads
// the plugin ONCE at boot — so a prompt can't be baked in (the system prompt
// varies per session via the platform env block, and changes on reload). The
// prompts are static between session Starts, so a live foci call every turn
// would be wasted work; foci writes the files once per Start and the plugin
// reads them locally. Mirrors the session-env plugin (session_env.go) exactly.
//
// foci also still sends its system prompt in the POST "system" field: that's
// the fallback if the system file is ever missing — the session keeps a prompt
// rather than running naked. When the file is present the plugin replaces the
// whole array, so there's no doubling. Compaction has no such fallback (opencode
// falls back to its own default template if the compaction file is missing).

package opencode

import (
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
	"foci/internal/tempdir"
)

const (
	sessionSystemSubdir  = "session-system"  // under tempdir
	sessionCompactSubdir = "session-compact" // under tempdir
	blankSystemPluginFn  = "blank-system.ts" // under .opencode/plugin/
)

// sessionSystemDir returns the tempdir subdirectory for per-session
// system-prompt files.
func sessionSystemDir() string {
	return filepath.Join(tempdir.Dir(), sessionSystemSubdir)
}

// sessionCompactDir returns the tempdir subdirectory for per-session
// compaction-prompt files.
func sessionCompactDir() string {
	return filepath.Join(tempdir.Dir(), sessionCompactSubdir)
}

// sessionSystemPath returns the system-prompt file path for one opencode session.
func sessionSystemPath(sessionID string) string {
	return filepath.Join(sessionSystemDir(), sessionID)
}

// sessionCompactPath returns the compaction-prompt file path for one session.
func sessionCompactPath(sessionID string) string {
	return filepath.Join(sessionCompactDir(), sessionID)
}

// writeSessionFile writes content to dir/sessionID (best-effort, logged). Shared
// by the system- and compaction-prompt writers. Empty sessionID/content is a
// no-op — a missing file makes the plugin leave opencode's default in place.
func writeSessionFile(kind, dir, sessionID, content string) {
	if sessionID == "" || content == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warnf("opencode", "%s: mkdir %s: %v", kind, dir, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID), []byte(content), 0o644); err != nil {
		log.Warnf("opencode", "%s: write %s: %v", kind, sessionID, err)
		return
	}
	log.Debugf("opencode", "%s: wrote prompt for session %s (%d bytes)", kind, sessionID, len(content))
}

// WriteSessionSystemFile writes foci's resolved system prompt for a session to
// the file the blank-system plugin reads. Called at Start (create / resume /
// compaction-bounce) — exactly when the prompt is (re)resolved from disk.
func WriteSessionSystemFile(sessionID, prompt string) {
	writeSessionFile("session-system", sessionSystemDir(), sessionID, prompt)
}

// WriteSessionCompactFile writes foci's resolved compaction-summary prompt for a
// session to the file the blank-system plugin's session.compacting hook reads.
func WriteSessionCompactFile(sessionID, prompt string) {
	writeSessionFile("session-compact", sessionCompactDir(), sessionID, prompt)
}

// RemoveSessionSystemFile removes the per-session system-prompt file. Best-effort.
func RemoveSessionSystemFile(sessionID string) {
	if sessionID == "" {
		return
	}
	_ = os.Remove(sessionSystemPath(sessionID))
}

// RemoveSessionCompactFile removes the per-session compaction-prompt file. Best-effort.
func RemoveSessionCompactFile(sessionID string) {
	if sessionID == "" {
		return
	}
	_ = os.Remove(sessionCompactPath(sessionID))
}

// EnsureBlankSystemPlugin writes the prompt plugin into the agent workspace if
// missing or stale. Must run BEFORE the server is acquired (opencode loads
// plugins at subprocess startup). Idempotent and best-effort.
func EnsureBlankSystemPlugin(workDir string) {
	ensureWorkspacePlugin(workDir, blankSystemPluginFn, blankSystemPluginSource(sessionSystemDir(), sessionCompactDir()))
}

// blankSystemPluginSource returns the TypeScript source for the prompt plugin,
// templated with the per-session file directories so the plugin has no runtime
// dependency on opencode's environment. Each hook reads <dir>/<sessionID>:
// system.transform replaces the system array (dropping opencode's default) when
// the file is present; session.compacting sets output.prompt when its file is
// present. A missing file leaves opencode's default for that hook untouched.
func blankSystemPluginSource(sysDir, compactDir string) string {
	const tmpl = `// Auto-generated by foci. Replaces opencode's built-in default system prompt
// and compaction template with foci's per-session prompts, read from files foci
// writes at session start. Do not edit; foci rewrites this on session start.
export default function() {
  const sysDir = {{SYS_DIR}};
  const compactDir = {{COMPACT_DIR}};
  const read = async (dir, sid) => {
    try {
      if (!sid) return null;
      const f = Bun.file(dir + "/" + sid);
      if (await f.exists()) return await f.text();
    } catch (e) {}
    return null;
  };
  return {
    "experimental.chat.system.transform": async (input, output) => {
      const p = await read(sysDir, input && input.sessionID);
      if (p !== null) { output.system.length = 0; output.system.push(p); }
    },
    "experimental.session.compacting": async (input, output) => {
      const p = await read(compactDir, input && input.sessionID);
      if (p !== null) output.prompt = p;
    }
  };
}
`
	s := strings.ReplaceAll(tmpl, "{{SYS_DIR}}", "`"+sysDir+"`")
	return strings.ReplaceAll(s, "{{COMPACT_DIR}}", "`"+compactDir+"`")
}
