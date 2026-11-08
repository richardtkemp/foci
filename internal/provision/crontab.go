package provision

import (
	"context"
	"fmt"
	"os"
	"strings"

	"foci/internal/usercrontab"
)

// GenerateCrontab reads a crontab template, replaces placeholders, strips comments,
// and staggers minute values based on existingAgentCount.
func GenerateCrontab(templatePath string, spec AgentSpec, existingAgentCount int) ([]string, error) {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read crontab template: %w", err)
	}
	tmpl := string(data)

	workspace := spec.workspacePath()

	// Replace placeholders
	tmpl = strings.ReplaceAll(tmpl, "AGENT_NAME", spec.ID)
	tmpl = strings.ReplaceAll(tmpl, "WORKSPACE", workspace)
	tmpl = strings.ReplaceAll(tmpl, "HOMEDIR", spec.HomeDir)

	offset := existingAgentCount * 3

	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(tmpl), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if offset > 0 {
			line = StaggerCrontabLine(line, offset)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// StaggerCrontabLine offsets the minute field(s) of a crontab line.
// Handles both simple ("0") and interval ("*/30") minute fields.
func StaggerCrontabLine(line string, offset int) string {
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return line // not a valid crontab line
	}
	minute := fields[0]
	if strings.HasPrefix(minute, "*/") {
		// Interval field like */30 — leave interval, will naturally stagger
		return line
	}
	// Absolute minute: add offset and wrap at 60
	var min int
	if _, err := fmt.Sscanf(minute, "%d", &min); err == nil {
		min = (min + offset) % 60
		fields[0] = fmt.Sprintf("%d", min)
		return strings.Join(fields, " ")
	}
	return line
}

// AppendCrontab appends entries to the service user's crontab: it reads the
// current crontab and installs it with the lines added, through usercrontab
// (stdin, no shell; each command time-limited, #2976). A variable so tests
// can replace it and never touch the real crontab.
var AppendCrontab = func(lines []string) error {
	return usercrontab.Append(context.Background(), lines)
}
