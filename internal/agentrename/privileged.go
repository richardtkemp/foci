package agentrename

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"foci/internal/procx"
)

// Privileged runs a command with root privileges through the host's real
// approval flow (sudo; on an aisudo host the approver taps approve/deny).
// stdin is fed to the command; its stdout is returned. Nothing here embeds a
// password or bypasses the approval: a declined or failed approval is an
// error, which the rename reports as a PENDING step.
type Privileged interface {
	Run(argv []string, stdin []byte) ([]byte, error)
}

// SudoRunner is the production Privileged: it runs `<Bin> argv...`. Bin is
// resolved on this process's PATH (default "sudo"). The approval prompt, if
// any, goes to the terminal on stderr.
type SudoRunner struct{ Bin string }

// Run implements Privileged.
func (s SudoRunner) Run(argv []string, stdin []byte) ([]byte, error) {
	bin := s.Bin
	if bin == "" {
		bin = "sudo"
	}
	cmd := procx.Spawn(context.Background(), procx.Operator, bin, argv...)
	// sudo stays in the terminal's foreground process group (Spawn puts a
	// child in its own group), so a password prompt on /dev/tty can read.
	cmd.SysProcAttr.Setpgid = false
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", bin, strings.Join(argv, " "), err)
	}
	return out, nil
}

// CrontabIO reads the current user's crontab (no privilege needed to read
// one's own). An absent crontab reads as "".
type CrontabIO interface {
	Read() (string, error)
}

// SystemCrontab reads with `crontab -l`.
type SystemCrontab struct{}

// Read implements CrontabIO.
func (SystemCrontab) Read() (string, error) {
	cmd := procx.Spawn(context.Background(), procx.Operator, "crontab", "-l")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(strings.ToLower(stderr.String()), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// rewriteSecrets returns secrets.toml content with every exact reference to
// agent old renamed: the per-agent table headers [agents.<old>] and
// [agents.<old>.<section>] (bare or quoted id), and each exact "<old>"
// element of an allowed_agents / denied_agents list (one-line or multi-line).
// Everything else — values, comments, layout — is byte for byte the same.
func rewriteSecrets(content, old, new string) string {
	header := regexp.MustCompile(`^(\s*\[\s*agents\.)("?)` + regexp.QuoteMeta(old) + `("?)(\s*[.\]])`)
	listKey := regexp.MustCompile(`^\s*(allowed_agents|denied_agents)\s*=`)
	elem := regexp.MustCompile(`"` + regexp.QuoteMeta(old) + `"|'` + regexp.QuoteMeta(old) + `'`)
	lines := strings.Split(content, "\n")
	inList := false
	for i, line := range lines {
		if m := header.FindStringSubmatch(line); m != nil && m[2] == m[3] {
			lines[i] = header.ReplaceAllString(line, "${1}${2}"+new+"${3}${4}")
			continue
		}
		if listKey.MatchString(line) {
			inList = true
		}
		if inList {
			code, comment := splitComment(line)
			lines[i] = elem.ReplaceAllStringFunc(code, func(s string) string { return s[:1] + new + s[:1] }) + comment
			if strings.Contains(code, "]") {
				inList = false
			}
		}
	}
	return strings.Join(lines, "\n")
}

// splitComment splits a TOML line at a '#' outside quotes.
func splitComment(line string) (code, comment string) {
	var q byte
	for j := 0; j < len(line); j++ {
		c := line[j]
		switch {
		case q == '"' && c == '\\':
			j++
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#':
			return line[:j], line[j:]
		}
	}
	return line, ""
}

// secretsSedScript is the GNU sed program a person runs by hand for a pending
// secrets.toml step: the same header and one-line list rewrites as
// rewriteSecrets (a multi-line list must be edited by hand).
func secretsSedScript(old, new string) string {
	o := regexp.QuoteMeta(old)
	return fmt.Sprintf(`s/^([[:space:]]*\[[[:space:]]*agents\.)("?)%s("?)([[:space:]]*[].])/\1\2%s\3\4/; /^[[:space:]]*(allowed|denied)_agents[[:space:]]*=/ s/"%s"/"%s"/g`, o, new, o, new)
}

// cronAgentFlag matches an exact agent reference on a foci command line:
// "-a <old>", "--agent <old>", "-a=<old>", "FOCI_AGENT=<old>", optionally
// quoted, ending at a word boundary of the shell (so -a bob never matches
// -a bobby or -a bob-2).
func cronAgentFlag(old string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s;&|(])(-a\s+|-a=|--agent\s+|--agent=|FOCI_AGENT=)(["']?)` + regexp.QuoteMeta(old) + `(["']?)($|[\s;&|)<>])`)
}

// rewriteCronLine renames the exact agent references on one crontab line,
// and in a move the workspace paths. A commented-out job is rewritten too:
// a paused "# … foci send -a <old>" would otherwise fail silently when it is
// re-enabled. ok reports a change.
func rewriteCronLine(line string, r ids, moveFrom, moveTo string) (string, bool) {
	if strings.TrimSpace(line) == "" {
		return line, false
	}
	out := line
	if strings.Contains(line, "foci") || strings.Contains(line, "FOCI_AGENT") {
		re := cronAgentFlag(r.old)
		// Matches can abut ("-a bob;-a bob"): repeat until stable.
		for {
			next := re.ReplaceAllString(out, "${1}${2}${3}"+r.new+"${4}${5}")
			if next == out {
				break
			}
			out = next
		}
	}
	if moveFrom != "" {
		out = pathPrefixRe(moveFrom).ReplaceAllString(out, strings.ReplaceAll(moveTo, "$", "$$")+"${1}")
		// A path relative to cron's working directory, the install root
		// ("-mf <old>/prompts/x.md"), names the moved workspace too.
		rel := regexp.MustCompile(`(^|[\s'"=])` + regexp.QuoteMeta(r.old) + `/`)
		out = rel.ReplaceAllString(out, "${1}"+r.new+"/")
	}
	return out, out != line
}

// pathPrefixRe matches dir as a whole path or path prefix (never /x/bobby for /x/bob).
func pathPrefixRe(dir string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(dir) + `($|[/"'\s;&|)])`)
}

// rewriteCrontab applies rewriteCronLine to every line.
func rewriteCrontab(content string, r ids, moveFrom, moveTo string) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		lines[i], _ = rewriteCronLine(l, r, moveFrom, moveTo)
	}
	return strings.Join(lines, "\n")
}
