package agentrename

import (
	"regexp"
	"strings"
)

// removeSecrets returns secrets.toml content with agent id removed (#3210):
// every [agents.<id>] and [agents.<id>.<section>] table (bare or quoted id),
// header to the next table header, and each exact "<id>" element of an
// allowed_agents / denied_agents list (one-line, multi-line or the
// comma-separated string form). removed holds the lines taken out whole
// (the tables), for the backup. Removing an element keeps a list's meaning
// for every other agent: an allowed_agents list left empty still allows
// nobody else, a denied_agents list left empty denies nobody else.
func removeSecrets(content, id string) (next string, removed []string) {
	header := regexp.MustCompile(`^(\s*\[\s*agents\.)("?)` + regexp.QuoteMeta(id) + `("?)(\s*[.\]])`)
	anyHeader := regexp.MustCompile(`^\s*\[`)
	listKey := regexp.MustCompile(`^\s*(allowed_agents|denied_agents)\s*=`)
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inTable, inList := false, false
	for _, line := range lines {
		if anyHeader.MatchString(line) {
			m := header.FindStringSubmatch(line)
			inTable = m != nil && m[2] == m[3]
		}
		if inTable {
			removed = append(removed, line)
			continue
		}
		if listKey.MatchString(line) {
			code, comment := splitComment(line)
			if _, val, _ := strings.Cut(code, "="); !strings.HasPrefix(strings.TrimSpace(val), "[") {
				out = append(out, removeFromStringList(code, id)+comment)
				continue
			}
			inList = true
		}
		if inList {
			code, comment := splitComment(line)
			closes := strings.Contains(code, "]")
			newCode := removeListElement(code, id)
			inList = !closes
			if strings.TrimSpace(newCode) == "" && strings.TrimSpace(code) != "" && strings.TrimSpace(comment) == "" {
				continue // a multi-line list's own "<id>", line: gone whole
			}
			out = append(out, newCode+comment)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), removed
}

// removeListElement removes each exact "<id>" / '<id>' element, with its
// separating comma, from the code part of one line of a TOML array.
func removeListElement(code, id string) string {
	q := regexp.QuoteMeta(id)
	el := `(?:"` + q + `"|'` + q + `')`
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(el + `\s*,\s*`), // "<id>", ...
		regexp.MustCompile(`\s*,\s*` + el), // ..., "<id>"
		regexp.MustCompile(el),             // the only element
	} {
		code = re.ReplaceAllString(code, "")
	}
	return code
}

// removeFromStringList removes each exact id entry of a string-form list,
// given the line's code: key = "a, id, b". Anything else is unchanged.
func removeFromStringList(code, id string) string {
	start := strings.IndexAny(code, `"'`)
	if start < 0 {
		return code
	}
	end := strings.LastIndexByte(code, code[start])
	if end <= start {
		return code
	}
	var keep []string
	for _, item := range strings.Split(code[start+1:end], ",") {
		if strings.TrimSpace(item) != id {
			keep = append(keep, strings.TrimSpace(item))
		}
	}
	return code[:start+1] + strings.Join(keep, ", ") + code[end:]
}

// removeCrontab returns the crontab with every line that belongs to agent id
// removed (#3210): a foci line naming it exactly (-a <id>, --agent <id>,
// FOCI_AGENT=<id>), or a line naming a path under the agent's workspace ws
// (which goes to the trash, so the job could only fail) when ws is set.
// Commented-out jobs are removed too: re-enabled, they would fail silently.
func removeCrontab(content, id, ws string) (next string, removed []string) {
	flag := cronAgentFlag(id)
	var wsRe *regexp.Regexp
	if ws != "" {
		wsRe = pathPrefixRe(ws)
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		owned := (strings.Contains(l, "foci") || strings.Contains(l, "FOCI_AGENT")) && flag.MatchString(l)
		if !owned && wsRe != nil && wsRe.MatchString(l) {
			owned = true
		}
		if owned && strings.TrimSpace(l) != "" {
			removed = append(removed, l)
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), removed
}
