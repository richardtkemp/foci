package workspace

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/session"
)

// DailyDigestMaxHeadingChars caps the heading text kept from ONE daily file.
// Measured on clutch (#2827, 30 days): a today+yesterday digest was median
// 2.7k chars, max 5.4k, so the cap only bites on a pathological file. When it
// bites, the most recent (last) headings are kept.
const DailyDigestMaxHeadingChars = 6000

// DailyMemoryDigest returns the markdown headings of the agent's daily memory
// files for now's date and the day before (memory/YYYY-MM-DD.md under
// workspaceDir, dates in now's location), as one system-prompt section. It
// returns "" when neither file has a heading. The result is a pure function
// of the two files and the two dates (no clock time), so two builds over
// unchanged files are byte-identical and the compaction prompt fingerprint
// only moves when a heading changes (#2827).
func DailyMemoryDigest(workspaceDir string, now time.Time) string {
	days := []time.Time{now, now.AddDate(0, 0, -1)}
	var sections []string
	for _, d := range days {
		rel := filepath.Join("memory", d.Format("2006-01-02")+".md")
		data, err := os.ReadFile(filepath.Join(workspaceDir, rel))
		if err != nil {
			continue
		}
		heads, dropped := capHeadings(markdownHeadings(data), DailyDigestMaxHeadingChars)
		if len(heads) == 0 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "### %s\n", rel)
		if dropped > 0 {
			fmt.Fprintf(&b, "(%d earlier headings omitted)\n", dropped)
		}
		for _, h := range heads {
			b.WriteString("- ")
			b.WriteString(h)
			b.WriteByte('\n')
		}
		sections = append(sections, b.String())
	}
	if len(sections) == 0 {
		return ""
	}
	return "## Recent Memory (headings only)\n" +
		"The section headings of today's and yesterday's daily memory files, as they were when this session started. " +
		"The files keep growing; to read a section, grep the file or use foci_memory_search.\n\n" +
		strings.TrimRight(strings.Join(sections, "\n"), "\n")
}

// markdownHeadings returns the ATX heading lines of a markdown document
// ("#" to "######" followed by a space), trimmed, skipping fenced code blocks
// so a shell comment inside ``` is not taken for a heading.
func markdownHeadings(data []byte) []string {
	var out []string
	inFence := false
	fence := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimLeft(line, " ")
		if inFence {
			if strings.HasPrefix(trimmed, fence) {
				inFence = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence, fence = true, trimmed[:3]
			continue
		}
		if len(line)-len(trimmed) > 3 { // indented code, not a heading
			continue
		}
		level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		if level < 1 || level > 6 || len(trimmed) == level || trimmed[level] != ' ' {
			continue
		}
		if h := strings.TrimSpace(trimmed); len(h) > level {
			out = append(out, h)
		}
	}
	return out
}

// capHeadings keeps the LAST headings whose total length fits in max chars
// and reports how many earlier ones it dropped.
func capHeadings(heads []string, max int) ([]string, int) {
	total := 0
	start := len(heads)
	for start > 0 {
		n := len(heads[start-1]) + 1
		if total+n > max {
			break
		}
		total += n
		start--
	}
	return heads[start:], start
}

// IsUserChatSession reports whether sessionKey names a root chat session
// (agent/c<chatID>): the conversation a human has with the agent. Branches
// (forks, spawns, batches, external branches) and independent sessions are
// not, so they never get the daily digest (Dick, 2026-11-03: "chats with you
// only").
func IsUserChatSession(sessionKey string) bool {
	k, err := session.ParseSessionKey(sessionKey)
	return err == nil && k.Type == 'c' && k.IsRoot()
}
