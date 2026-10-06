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

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
)

// runConfigCheck loads the config at path and returns the process exit code.
// It performs no side effects beyond reading the file and printing a verdict.
func runConfigCheck(path string, stdout, stderr io.Writer) int {
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

	if failures := checkPretoolExamples(cfg, stderr); failures > 0 {
		_, _ = fmt.Fprintf(stderr, "config check FAILED: %d pretool example failure(s)\n", failures)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "config check OK: "+path+" loads cleanly with no unknown keys")
	return 0
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
