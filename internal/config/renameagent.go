package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AgentRename describes one agent-id rename of a foci.toml file, for
// PlanRenameAgent. The rename command (internal/agentrename) fills it from the
// loaded config and the user's workspace choice.
type AgentRename struct {
	Old, New string
	// PinBots lists the platform ids (telegram, discord) whose bot name the
	// agent resolves to Old today. Each gets an explicit bot = "<Old>" unless
	// its [[agents.platforms]] block already names a bot, so the renamed agent
	// keeps the same bot and bot token (the bot otherwise defaults to the id).
	PinBots []string
	// KeepWorkspace, when non-empty, is written as workspace = "<path>" in the
	// agent's block if the block has no workspace key: the workspace default is
	// <install root>/<id>, so without it the renamed agent would look in ~/<New>.
	KeepWorkspace string
	// MoveFrom/MoveTo, when both are set, rewrite every non-comment occurrence
	// of the path MoveFrom (as a whole path or a path prefix) to MoveTo.
	MoveFrom, MoveTo string
}

// agentListKeys are the foci.toml keys whose value is a list of agent ids.
var agentListKeys = map[string]bool{"allowed_agents": true, "denied_agents": true, "agents": true}

// PlanRenameAgent computes the foci.toml content after renaming agent r.Old
// to r.New, preserving comments and layout. It changes: the [[agents]] id,
// a master_agent and an [askgw] default_agent equal to Old, every exact "Old"
// element of an allowed_agents/denied_agents/agents list, the bot pins
// (r.PinBots), the kept workspace (r.KeepWorkspace) and the moved paths
// (r.MoveFrom → r.MoveTo). Pure: it touches no disk. The caller checks the
// result loads (CheckFileContent) before writing it.
func PlanRenameAgent(content string, r AgentRename) (string, error) {
	lines := strings.Split(content, "\n")
	if s, _ := findAgentBlock(lines, r.Old); s < 0 {
		return "", fmt.Errorf("agent %q not found in config file", r.Old)
	}
	if s, _ := findAgentBlock(lines, r.New); s >= 0 {
		return "", fmt.Errorf("agent %q already exists in config file", r.New)
	}

	// Pin the bots and keep the workspace while the block still says Old, so
	// both read the block the user wrote.
	var err error
	for _, p := range r.PinBots {
		if lines, err = pinAgentPlatformBot(lines, r.Old, p, r.Old); err != nil {
			return "", err
		}
	}
	if r.KeepWorkspace != "" {
		if lines, err = setAgentKeyIfAbsent(lines, r.Old, "workspace", strconv.Quote(r.KeepWorkspace)); err != nil {
			return "", err
		}
	}
	if _, lines, err = setInAgentBlock(lines, r.Old, "id", strconv.Quote(r.New)); err != nil {
		return "", err
	}
	if lines, err = replaceTopLevelIDValue(lines, "master_agent", r.Old, r.New); err != nil {
		return "", err
	}
	if lines, err = replaceSectionIDValue(lines, "askgw", "default_agent", r.Old, r.New); err != nil {
		return "", err
	}
	if lines, err = replaceAgentListElements(lines, r.Old, r.New); err != nil {
		return "", err
	}
	if r.MoveFrom != "" && r.MoveTo != "" {
		lines = replacePathRefs(lines, r.MoveFrom, r.MoveTo)
	}
	return strings.Join(lines, "\n"), nil
}

// PathRef is one line of a config file that names a path, for the plan.
type PathRef struct {
	Line int // 1-based
	Text string
}

// FindPathRefs lists the non-comment lines of content that name prefix as a
// whole path or a path prefix (the lines PlanRenameAgent's move rewrites).
func FindPathRefs(content, prefix string) []PathRef {
	re := pathRefRe(prefix)
	var out []PathRef
	for i, line := range strings.Split(content, "\n") {
		code, _ := splitLineComment(line)
		if re.MatchString(code) {
			out = append(out, PathRef{Line: i + 1, Text: strings.TrimSpace(line)})
		}
	}
	return out
}

// pathRefRe matches prefix as a whole path or path prefix: followed by a
// separator, a quote, a closing bracket, whitespace or the end — never by more
// name characters (/home/foci/bob must not match /home/foci/bobby).
func pathRefRe(prefix string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(prefix) + `($|[/"'\s,\]}])`)
}

// replacePathRefs rewrites from → to in the non-comment part of every line.
func replacePathRefs(lines []string, from, to string) []string {
	re := pathRefRe(from)
	for i, line := range lines {
		code, comment := splitLineComment(line)
		if !re.MatchString(code) {
			continue
		}
		lines[i] = re.ReplaceAllString(code, strings.ReplaceAll(to, "$", "$$")+"${1}") + comment
	}
	return lines
}

// splitLineComment splits one line at its comment ('#' outside a quoted
// string). A whole-line comment is all comment.
func splitLineComment(line string) (code, comment string) {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return "", line
	}
	var quote byte
	for j := 0; j < len(line); j++ {
		c := line[j]
		switch {
		case quote == '"' && c == '\\':
			j++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:j], line[j:]
		}
	}
	return line, ""
}

// replaceTopLevelIDValue sets key = "<to>" when the key sits before the first
// table header and its value is exactly from.
func replaceTopLevelIDValue(lines []string, key, from, to string) ([]string, error) {
	end := len(lines)
	top := topLevel(lines)
	for i := range lines {
		if top[i] && anySectionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return replaceIDValueIn(lines, 0, end, key, from, to)
}

// replaceSectionIDValue sets [section] key = "<to>" when its value is exactly from.
func replaceSectionIDValue(lines []string, section, key, from, to string) ([]string, error) {
	start, end := findSectionBounds(lines, section)
	if start < 0 {
		return lines, nil
	}
	return replaceIDValueIn(lines, start+1, end, key, from, to)
}

func replaceIDValueIn(lines []string, from, to int, key, oldID, newID string) ([]string, error) {
	re := keyLineRe(key)
	top := topLevel(lines)
	for i := from; i < to; i++ {
		if !top[i] || !re.MatchString(lines[i]) {
			continue
		}
		end, raw, err := valueSpan(lines, i)
		if err != nil {
			return nil, err
		}
		if v, err := decodeTOMLQuotedValue(raw); err != nil || v != oldID {
			return lines, nil
		}
		if end == i {
			// One line: swap the value in place, keeping a trailing comment.
			code, comment := splitLineComment(lines[i])
			k, v, _ := strings.Cut(code, "=")
			lines[i] = k + "=" + strings.Replace(v, raw, strconv.Quote(newID), 1) + comment
			return lines, nil
		}
		return replaceSpan(lines, i, end, key+" = "+strconv.Quote(newID)), nil
	}
	return lines, nil
}

// replaceAgentListElements rewrites every exact "<from>" / '<from>' element of
// an allowed_agents / denied_agents / agents list value.
func replaceAgentListElements(lines []string, from, to string) ([]string, error) {
	top := topLevel(lines)
	elem := regexp.MustCompile(`(["'])` + regexp.QuoteMeta(from) + `(["'])`)
	for i := 0; i < len(lines); i++ {
		if !top[i] {
			continue
		}
		k, _, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(lines[i]), "#") || !agentListKeys[strings.TrimSpace(k)] {
			continue
		}
		end, _, err := valueSpan(lines, i)
		if err != nil {
			return nil, err
		}
		for j := i; j <= end; j++ {
			code, comment := splitLineComment(lines[j])
			lines[j] = elem.ReplaceAllStringFunc(code, func(m string) string {
				if m[0] != m[len(m)-1] {
					return m
				}
				return string(m[0]) + to + string(m[0])
			}) + comment
		}
		i = end
	}
	return lines, nil
}

// setAgentKeyIfAbsent writes key = value inline in the agent's block unless
// the block already sets the key.
func setAgentKeyIfAbsent(lines []string, agentID, key, value string) ([]string, error) {
	loc, err := locateAgentKey(lines, agentID, key)
	if err != nil {
		return nil, err
	}
	if loc.found >= 0 {
		return lines, nil
	}
	start, end := findAgentBlock(lines, agentID)
	return insertAfterID(lines, start+1, end, agentIDLineRe(agentID), key+" = "+value), nil
}

// agentIDLineRe matches the id = "<id>" line of a block.
func agentIDLineRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*id\s*=\s*"` + regexp.QuoteMeta(id) + `"\s*(#.*)?$`)
}

// insertAfterID inserts line right after the block's id line (found in
// [from, to) by idRe), so a new key sits with the block's keys and no
// commented-out line is touched. Without an id line it goes at from.
func insertAfterID(lines []string, from, to int, idRe *regexp.Regexp, line string) []string {
	top := topLevel(lines)
	at := from
	for i := from; i < to; i++ {
		if top[i] && idRe.MatchString(lines[i]) {
			at = i + 1
			break
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at]...)
	out = append(out, line)
	return append(out, lines[at:]...)
}

// agentRegionEnd returns the end (exclusive) of an agent's whole region: its
// [[agents]] block plus every [agents.*] / [[agents.*]] sub-table after it.
func agentRegionEnd(lines []string, blockEnd int) int {
	top := topLevel(lines)
	i := blockEnd
	for i < len(lines) {
		if !top[i] {
			i++
			continue
		}
		header := subTableHeader(lines[i])
		if header == "" {
			break
		}
		next := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if top[j] && anySectionRe.MatchString(lines[j]) {
				next = j
				break
			}
		}
		i = next
	}
	return i
}

// subTableHeader returns the lower-cased name of an [agents.*] or
// [[agents.*]] header line ("agents.platforms"), or "" for any other line.
func subTableHeader(line string) string {
	for _, re := range []*regexp.Regexp{arrayHeaderRe, sectionHeaderRe} {
		if m := re.FindStringSubmatch(line); m != nil {
			h := strings.ToLower(strings.TrimSpace(m[1]))
			if strings.HasPrefix(h, "agents.") {
				return h
			}
			return ""
		}
	}
	return ""
}

// pinAgentPlatformBot makes the agent's platform block for platformID name
// bot explicitly: a [[agents.platforms]] block with that id and no bot key
// gets bot = "<bot>"; with no such block, a new block is appended to the
// agent's region. A block that already names a bot is left alone.
func pinAgentPlatformBot(lines []string, agentID, platformID, bot string) ([]string, error) {
	start, blockEnd := findAgentBlock(lines, agentID)
	if start < 0 {
		return nil, fmt.Errorf("agent %q not found in config file", agentID)
	}
	regionEnd := agentRegionEnd(lines, blockEnd)
	top := topLevel(lines)
	idRe := agentIDLineRe(platformID)
	botRe := keyLineRe("bot")
	for i := blockEnd; i < regionEnd; i++ {
		if !top[i] || subTableHeader(lines[i]) != "agents.platforms" || !arrayHeaderRe.MatchString(lines[i]) {
			continue
		}
		bodyEnd := regionEnd
		for j := i + 1; j < regionEnd; j++ {
			if top[j] && anySectionRe.MatchString(lines[j]) {
				bodyEnd = j
				break
			}
		}
		isTarget, hasBot := false, false
		for j := i + 1; j < bodyEnd; j++ {
			if !top[j] {
				continue
			}
			isTarget = isTarget || idRe.MatchString(lines[j])
			hasBot = hasBot || botRe.MatchString(lines[j])
		}
		if !isTarget {
			continue
		}
		if hasBot {
			return lines, nil
		}
		return insertAfterID(lines, i+1, bodyEnd, idRe, "bot = "+strconv.Quote(bot)), nil
	}
	block := []string{"[[agents.platforms]]", "id = " + strconv.Quote(platformID), "bot = " + strconv.Quote(bot)}
	at := regionEnd
	for at > start+1 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	ins := append([]string{""}, block...)
	if at < len(lines) && strings.TrimSpace(lines[at]) != "" {
		ins = append(ins, "")
	}
	out := make([]string, 0, len(lines)+len(ins))
	out = append(out, lines[:at]...)
	out = append(out, ins...)
	return append(out, lines[at:]...), nil
}

// RenameAgentCommand is the command line that renames agent oldID, as the
// places that refuse an id edit (the app config editor) tell the user.
func RenameAgentCommand(oldID string) string {
	if oldID == "" {
		oldID = "<old-id>"
	}
	return "foci rename-agent " + oldID + " <new-id>"
}

// DefaultReplayStoreFile is the app's durable replay-frame DB file, relative
// to the data dir, when [platforms.app] replay_store_path is unset.
const DefaultReplayStoreFile = "app-frames.db"
