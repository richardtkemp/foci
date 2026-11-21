package config

import (
	"fmt"
	"strings"
)

// PlanDeleteAgent computes the foci.toml content after deleting agent id
// (#3210): its whole region — the [[agents]] block and every [agents.*] /
// [[agents.*]] sub-table after it — is removed, with the blank lines around
// it collapsed to one. Every other line, comment and layout is kept. Pure: it
// touches no disk. The caller checks the result loads (CheckFileContent)
// before writing it, and refuses first when another key names the agent
// (AgentDeleteBlockers).
func PlanDeleteAgent(content, id string) (string, error) {
	lines := strings.Split(content, "\n")
	start, blockEnd := findAgentBlock(lines, id)
	if start < 0 {
		return "", fmt.Errorf("agent %q not found in config file", id)
	}
	end := agentRegionEnd(lines, blockEnd)
	// Trailing blank lines of the region stay as the separator to what
	// follows; strip the region's own so at most one blank line remains.
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := make([]string, 0, len(lines)-(end-start))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	// Collapse the blank run left where the region was (before it and after it).
	if start > 0 && start < len(out) && strings.TrimSpace(out[start-1]) == "" && strings.TrimSpace(out[start]) == "" {
		j := start
		for j < len(out) && strings.TrimSpace(out[j]) == "" {
			j++
		}
		out = append(out[:start], out[j:]...)
	}
	return strings.Join(out, "\n"), nil
}

// AgentDeleteBlockers lists the settings outside agent id's own block that
// name it as their agent, each as "<key> = \"<id>\"". An agent is not
// deleted while one does (#3210, Dick 2026-11-21): the user changes the key
// first, so foci never picks a replacement agent on its own.
func AgentDeleteBlockers(cfg *Config, id string) []string {
	var out []string
	if cfg.MasterAgent == id {
		out = append(out, fmt.Sprintf("master_agent = %q", id))
	}
	if cfg.Askgw.DefaultAgent == id {
		out = append(out, fmt.Sprintf("[askgw] default_agent = %q", id))
	}
	if pl := cfg.Platform("slack"); pl != nil && pl.Slack != nil {
		if pl.Slack.DefaultAgent == id {
			out = append(out, fmt.Sprintf("[platforms.slack] default_agent = %q", id))
		}
		for _, ch := range pl.Slack.Channels {
			if ch.Agent == id {
				out = append(out, fmt.Sprintf("[platforms.slack] channel %s agent = %q", ch.ID, id))
			}
		}
	}
	return out
}
