package agentrename

import (
	"context"
	"regexp"
	"strings"

	"foci/internal/usercrontab"
)

// CrontabIO reads and installs the crontab the rename edits: the gateway
// user's own, which is the user that installs foci's cron jobs (no privilege
// is needed for either). An absent crontab reads as "".
type CrontabIO interface {
	Read() (string, error)
	Install(content string) error
}

// SystemCrontab is the production CrontabIO: `crontab -l` and `crontab -`
// through usercrontab, each time-limited (#2976).
type SystemCrontab struct{}

// Read implements CrontabIO.
func (SystemCrontab) Read() (string, error) {
	return usercrontab.Read(context.Background())
}

// Install implements CrontabIO.
func (SystemCrontab) Install(content string) error {
	return usercrontab.Install(context.Background(), content)
}

// rewriteSecrets returns secrets.toml content with every exact reference to
// agent old renamed: the per-agent table headers [agents.<old>] and
// [agents.<old>.<section>] (bare or quoted id), and each exact "<old>"
// element of an allowed_agents / denied_agents list (one-line, multi-line or
// the comma-separated string form secrets.toml also accepts).
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
			code, comment := splitComment(line)
			if _, val, _ := strings.Cut(code, "="); !strings.HasPrefix(strings.TrimSpace(val), "[") {
				lines[i] = rewriteStringList(code, old, new) + comment
				continue
			}
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

// rewriteStringList renames each exact old entry of a string-form list, given
// the line's code: key = "a, old, b". Anything else on the line is unchanged.
func rewriteStringList(code, old, new string) string {
	start := strings.IndexAny(code, `"'`)
	if start < 0 {
		return code
	}
	end := strings.LastIndexByte(code, code[start])
	if end <= start {
		return code
	}
	items := strings.Split(code[start+1:end], ",")
	for i, item := range items {
		if strings.TrimSpace(item) == old {
			items[i] = strings.Replace(item, old, new, 1)
		}
	}
	return code[:start+1] + strings.Join(items, ",") + code[end:]
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
