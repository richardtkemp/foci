package main

// checkconfig.go implements `foci-gw -check-config`: a pre-flight validation
// of the config file that runs WITHOUT starting the server, opening databases,
// rotating the production log, or creating any directories.
//
// Purpose: update.sh builds a new foci-gw binary, then runs it with this flag
// (as root, against each service's -config path) BEFORE stopping the running
// daemon. If the new binary cannot load a config (a parse/validate error, or —
// under the strict policy — any unknown/deprecated key such as a renamed
// setting), the check exits non-zero and update.sh aborts, leaving the running
// foci untouched. Without this, config incompatibilities were only discovered
// after the old daemon had already been replaced, bricking the service.
//
// Besides the load verdict, the check previews the auto-approving that will
// stop, so a deploy can be judged before it lands (#1897):
//
//   - every auto_approve entry the startup drop will ignore, per scope (the
//     global [permissions] block or the agent by id), with the reason and the
//     path execguard gave;
//   - every entry naming a command on no directory of PATH, kept but approved
//     unchecked;
//   - every built-in group command (read-only, safe-write) the match-time
//     veto will refuse, with the reason and the path — a vetoed built-in
//     otherwise just falls through to an approval prompt silently;
//   - one summary line with the count and the sorted command names.
//
// All of those are WARNINGS: the finding is identical before and after the
// deploy, so blocking the deploy would fix nothing (the backend_config
// warning follows the same rule).
//
// The preview judges the PATH an agent shell of the user running the check
// will search, derived exactly as startup derives it (shellenv, then
// procx.SetOperatorEnv, then execguard.Live) — with two fail-closed
// refusals startup does not have, both reported as one "auto_approve was
// not checked" warning:
//
//   - as ROOT nothing is checked and the rc file is never sourced: root's
//     access(2) says every file is writable, so the preview would flag every
//     command, and sourcing a foci-writable file as root is forbidden (the
//     same rule as the #2039 when-scripts);
//   - as a user, the rc file is sourced only when the file and every
//     directory on its path is writable only by the invoking user and root —
//     not group-writable, not world-writable, not another user's, not
//     reached through a symlink, and not a bare relative name (bash searches
//     PATH for those). #2347 owns the full trust rule; this is the
//     fail-closed subset.
//
// Exit codes:
//
//	0  config loads cleanly and has no unknown keys — the new binary will start.
//	   backend_config keys the agent's backend ignores are listed as a
//	   warning but do not fail the check: unlike a renamed key, the upgrade
//	   loses nothing (they were never honoured), so they must not block it.
//	   A pretool rule's failing example (of a rule without a when-script)
//	   DOES fail the check (#2039): the rule no longer denies what it was
//	   written to deny. Rules with a when-script get a patterns-only WARN
//	   instead — check-config runs as root and must never run one.
//	1  parse/validate error, OR one or more unknown/deprecated keys (strict),
//	   OR a failing pretool example, OR no file at the path (-config, else
//	   $FOCI_CONFIG, else ~/config/foci.toml)
//	2  usage error (e.g. config path unreadable for reasons other than load)
//
// Policy: STRICT. A silently-renamed key (old name still present in the file)
// is a real incompatibility — the old setting is lost on startup with only a
// warning — so unknown keys block the upgrade just like a hard load failure.
//
// Internal config.Load warnings spill to stderr via the default (pre-Init)
// logger; this is intentional and harmless — the production log is never opened.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
	"foci/internal/execguard"
	"foci/internal/procx"
	"foci/internal/shellenv"
)

// checkDeps is everything the auto-approve preview needs from the outside
// world, injected so its tests never depend on the host's real effective
// UID, PATH, rc file or filesystem. prodCheckDeps is the production wiring.
// It is a package VALUE, never written: the package's tests run heavily in
// parallel, and a swappable seam would let one test's fake race another
// test's runConfigCheck.
type checkDeps struct {
	euid           func() int
	loadEnvFile    func(cfg *string) (map[string]string, error) // shellenv.LoadGated + the trust gate; an error is the gate's refusal
	setOperatorEnv func(map[string]string)
	probe          func() execguard.Env
}

// prodCheckDeps mirrors main()'s derivation: the same loader (shellenv, with
// check-config's trust gate between Resolve and Capture), the same overlay
// install (procx.SetOperatorEnv), the same view (execguard.Live, which reads
// the operator population by name). The preview therefore judges exactly the
// PATH startup judges, minus what the gate refused to source.
var prodCheckDeps = checkDeps{
	euid: os.Geteuid,
	loadEnvFile: func(cfg *string) (map[string]string, error) {
		env, _, _, err := shellenv.LoadGated(cfg, checkEnvFileGate)
		return env, err
	},
	setOperatorEnv: procx.SetOperatorEnv,
	probe:          execguard.Live,
}

// runConfigCheck loads the config at path and returns the process exit code.
// It performs no side effects beyond reading the file and printing a verdict.
func runConfigCheck(path string, stdout, stderr io.Writer) int {
	return runConfigCheckWith(path, stdout, stderr, prodCheckDeps)
}

func runConfigCheckWith(path string, stdout, stderr io.Writer, d checkDeps) int {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		// Say it's the path, not the config, that's wrong: a bare "no such
		// file" from a stray default read like the live config was broken (#2117).
		_, _ = fmt.Fprintf(stderr, "config check FAILED: no config file at %s\n", path)
		_, _ = fmt.Fprintf(stderr, "Point at one explicitly with -config PATH, e.g. foci-gw -check-config -config /home/foci/config/foci.toml\n")
		return 1
	}

	cfg, err := config.Load(path, delegator.RegisteredNames())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config check FAILED: %v\n", err)
		return 1
	}

	if len(cfg.UndefinedKeys) > 0 {
		_, _ = fmt.Fprintf(stderr, "config check FAILED: %d unknown/deprecated key(s) in %s:\n", len(cfg.UndefinedKeys), path)
		for _, k := range cfg.UndefinedKeys {
			_, _ = fmt.Fprintf(stderr, "  - %s\n", k)
		}
		_, _ = fmt.Fprintf(stderr, "These keys are silently ignored at startup (a rename loses the old setting). Fix or remove them before upgrading.\n")
		return 1
	}

	if ignored := ignoredBackendConfig(cfg); len(ignored) > 0 {
		_, _ = fmt.Fprintf(stderr, "config check warning: %d backend_config key(s) the agent's backend ignores:\n", len(ignored))
		for _, k := range ignored {
			_, _ = fmt.Fprintf(stderr, "  - %s\n", k)
		}
	}

	plan := planAutoApproveCheck(cfg, d)
	if plan.skip != "" {
		_, _ = fmt.Fprintf(stderr, "config check warning: auto_approve was not checked: %s\n", plan.skip)
	} else {
		reportAutoApproveWarnings(stderr, cfg, plan.env)
	}

	if failures := checkPretoolExamples(cfg, stderr); failures > 0 {
		_, _ = fmt.Fprintf(stderr, "config check FAILED: %d pretool example failure(s)\n", failures)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "config check OK: "+path+" loads cleanly with no unknown keys")
	return 0
}

// autoApproveCheckPlan is the outcome of deriving the view the preview will
// judge auto-approve commands against.
type autoApproveCheckPlan struct {
	skip string        // the single not-checked warning; empty when the check ran
	env  execguard.Env // the view to judge; zero when skip != ""
}

// planAutoApproveCheck derives that view exactly as main() does — load the
// operator env file, install it as the operator population, then read
// execguard's live view — never its own PATH resolution, with the two
// fail-closed refusals described in the file header.
//
// Every silent branch (home error, nothing to load, capture failure) mirrors
// shellenv.Load's own handling, which WARNs on its side and leaves the
// operator and trusted populations identical: the preview then judges the
// same PATH startup would judge. A missing explicit file is one of those
// states — startup's capture of it would fail — but is never sourced by the
// check itself, so nothing that appears in the window between check and
// source is run.
func planAutoApproveCheck(cfg *config.Config, d checkDeps) autoApproveCheckPlan {
	if d.euid() == 0 {
		return autoApproveCheckPlan{skip: "this check runs as root, where every file is writable, so it would flag every command. Run foci-gw -check-config as the service user to see which commands will stop auto-approving."}
	}
	env, refused := d.loadEnvFile(cfg.ShellEnvFile)
	if refused != nil && !errors.Is(refused, errEnvFileMissing) {
		return autoApproveCheckPlan{skip: refused.Error()}
	}
	d.setOperatorEnv(env)
	return autoApproveCheckPlan{env: d.probe()}
}

// errEnvFileMissing reports a configured shell_env_file that does not exist.
// Not a refusal: startup would attempt the source, fail, and run with no
// operator overlay, so the preview silently judges the same PATH.
var errEnvFileMissing = errors.New("shell env file does not exist")

// checkEnvFileGate is the shellenv.Gate -check-config installs: the file must
// be shown writable only by the invoking user and root before the check will
// source it (#2347 owns the full trust rule; this is the fail-closed
// subset). It also absolute-ises a relative path containing a slash — bash's
// `.` resolves such a path against the cwd, so walking and sourcing the
// absolute path judges the file the shell will run — and refuses a bare
// relative name outright: bash searches PATH for those, so the file judged
// could differ from the file run.
func checkEnvFileGate(path string) (string, error) {
	if !filepath.IsAbs(path) && !strings.Contains(path, "/") {
		return "", fmt.Errorf("shell_env_file %q is a bare relative name — bash sources the first match on PATH for such a name, so the check cannot know which file the shells will run. Give shell_env_file an absolute path", path)
	}
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot resolve the working directory to make shell_env_file %q absolute: %w", path, err)
		}
		path = filepath.Join(cwd, path)
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return "", errEnvFileMissing
	}
	if concern := envFileConcern(path, os.Geteuid()); concern != "" {
		return "", fmt.Errorf("%s. %s", concern, envFileRefusalRule)
	}
	return path, nil
}

// envFileRefusalRule names every shape the trust walk refuses. It is part of
// the user-facing warning, so it must stay true on ordinary machines: the
// common case is a group-writable home directory, and an earlier attempt's
// text named only a rare one (#1897 requirement 8).
const envFileRefusalRule = "-check-config will not source a shell env file unless the file and every directory on its path is writable only by the invoking user and root — not group-writable, not world-writable, not another user's, not reached through a symlink, and not a bare relative name (bash searches PATH for those). Fix the file or point shell_env_file somewhere safe, then re-run."

// envFileConcern reports why path cannot be shown writable only by uid and
// root, or "" when it can. Every component from the file up to the filesystem
// root is inspected: a writable or foreign-owned directory lets the file be
// replaced or planted even when the file itself is tight, and a symlink
// anywhere on the path means the file the shell runs is the target, which
// this walk did not judge. Lstat is used throughout — the link itself is the
// identity being judged.
func envFileConcern(path string, uid int) string {
	return envFileConcernIn(path, uid, "/")
}

// envFileConcernIn is envFileConcern bounded by stopAt: the walk covers path
// and every parent up to AND INCLUDING stopAt, then stops. Production passes
// "/" (see envFileConcern); tests pass a controlled chain's root so they do
// not depend on the ancestors of their temp directories.
func envFileConcernIn(path string, uid int, stopAt string) string {
	cur := path
	for {
		info, err := os.Lstat(cur)
		if err != nil {
			return fmt.Sprintf("cannot inspect %s: %v", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Sprintf("%s is a symlink, so the file the shells would run is its target, which the check did not judge", cur)
		}
		if info.Mode().Perm()&0o002 != 0 {
			return fmt.Sprintf("%s is world-writable (mode %04o)", cur, info.Mode().Perm())
		}
		if info.Mode().Perm()&0o020 != 0 {
			return fmt.Sprintf("%s is group-writable (mode %04o)", cur, info.Mode().Perm())
		}
		owner := info.Sys().(*syscall.Stat_t).Uid
		invoking := uint32(uid) // #nosec G115 - uids from Getuid/Geteuid are non-negative and fit in uint32
		if owner != 0 && owner != invoking {
			return fmt.Sprintf("%s is owned by uid %d, not the invoking user (uid %d) or root", cur, owner, uid)
		}
		if cur == stopAt {
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "" // reached / without hitting stopAt: nothing above to check
		}
		cur = parent
	}
}

// reportAutoApproveWarnings writes the auto-approve preview described in the
// file header: per-entry and per-command lines carrying the reason and the
// path execguard gave, then — when at least one entry or command will stop
// auto-approving — one summary line naming the sorted, de-duplicated command
// tokens from config entries and built-in groups together.
func reportAutoApproveWarnings(w io.Writer, cfg *config.Config, env execguard.Env) {
	var stopping []string
	for _, scope := range cfg.ScanAutoApproveRules(env) {
		for _, d := range scope.Dropped {
			_, _ = fmt.Fprintf(w, "config check warning: %s: auto_approve entry %q will be ignored at startup — %s (%s)\n",
				checkScopeLabel(scope), d.Rule, d.Reason, d.Path)
			stopping = append(stopping, d.Command)
		}
		for _, u := range scope.Unlocatable {
			_, _ = fmt.Fprintf(w, "config check warning: %s: auto_approve entry %q names %q, which is on no directory of PATH; it will be approved unchecked\n",
				checkScopeLabel(scope), u.Rule, u.Command)
		}
	}
	for _, b := range substitutableBuiltinsFor(cfg, env) {
		_, _ = fmt.Fprintf(w, "config check warning: built-in %s group: command %q will be vetoed at match time — %s (%s); it falls through to an approval prompt\n",
			b.Group, b.Command, b.Reason, b.Path)
		stopping = append(stopping, b.Command)
	}
	if len(stopping) == 0 {
		return
	}
	slices.Sort(stopping)
	stopping = slices.Compact(stopping)
	_, _ = fmt.Fprintf(w, "config check warning: %d command(s) will stop auto-approving under this config: %s\n",
		len(stopping), strings.Join(stopping, ", "))
}

// checkScopeLabel renders a scope for -check-config output. Globalness is
// the report's boolean, never a comparison of the rendered string: an agent
// whose id is "global" is labelled as that agent, and the global block is
// still labelled global (#1897 requirement 4).
func checkScopeLabel(scope config.AutoApproveScopeReport) string {
	if scope.Global {
		return "global"
	}
	return fmt.Sprintf("agents[%s]", scope.AgentID)
}

// checkPretoolExamples runs every agent's pretool rule examples (#2039) in
// WhenSkip mode and returns the number of hard failures. A failing example
// of a rule WITHOUT a when-script is a real incompatibility — the rule no
// longer denies what it was written to deny — and blocks the upgrade.
//
// A rule WITH a when-script is different: check-config runs as root during
// deploys and the scripts come from a config file the foci user can write,
// so no script may ever be executed here (pretool.WhenSkip is that
// guarantee). Such rules' examples are checked against the rule's patterns
// only, and each gets one WARN line; the exit status never changes for them.
func checkPretoolExamples(cfg *config.Config, stderr io.Writer) (failures int) {
	for _, a := range cfg.Agents {
		rules, _, _ := cfg.PreToolRules(a.ID)
		// whenWarned dedups the WARN per rule name; n counts its examples.
		type whenRule struct {
			examples, denyMisses int
		}
		whenWarned := map[string]*whenRule{}
		var order []string
		for _, o := range pretool.RunExamples(rules, pretool.WhenSkip) {
			if o.WhenUnknown {
				w, seen := whenWarned[o.Rule]
				if !seen {
					w = &whenRule{}
					whenWarned[o.Rule] = w
					order = append(order, o.Rule)
				}
				w.examples++
				if !o.Pass && o.Kind == pretool.ExampleKindDeny {
					w.denyMisses++
				}
				continue
			}
			if !o.Pass {
				failures++
				actual := o.Matched
				if actual == "" {
					actual = "nothing"
				}
				expected := o.Rule
				if o.Kind == pretool.ExampleKindAllow {
					expected = "no match by " + o.Rule
				}
				_, _ = fmt.Fprintf(stderr, "agents[%s].pretool_rules %q: %s example %s: expected %s, matched %s\n", a.ID, o.Rule, o.Kind, o.Example, expected, actual)
			}
		}
		for _, name := range order {
			w := whenWarned[name]
			line := fmt.Sprintf("config check warning: agents[%s].pretool_rules %q: when not evaluated by check-config — %d example(s) checked against patterns only", a.ID, name, w.examples)
			if w.denyMisses > 0 {
				line += fmt.Sprintf("; %d deny example(s) do not match the rule's patterns", w.denyMisses)
			}
			_, _ = fmt.Fprintln(stderr, line)
		}
	}
	return failures
}

// ignoredBackendConfig lists every [agents.backend_config] key an agent sets
// that its backend does not read (#2178), e.g. "agents[helen].backend_config.
// hostname (backend claude-code does not read it)". Without it such a key is
// accepted and silently has no effect.
func ignoredBackendConfig(cfg *config.Config) []string {
	var out []string
	for _, a := range cfg.Agents {
		for _, k := range delegator.IgnoredConfigKeys(a.Backend, a.BackendConfig.SetKeys()) {
			out = append(out, fmt.Sprintf("agents[%s].backend_config.%s", a.ID, k))
		}
	}
	return out
}
