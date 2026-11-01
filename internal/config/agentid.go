package config

import (
	"fmt"
	"regexp"
)

// agentIDRe is the one agent-id format rule: a lowercase slug starting with a
// letter. It also keeps "/" (the session-key delimiter) and a leading dot out.
var agentIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// IsValidAgentID reports whether id has the agent-id format (lowercase slug).
// It does not check the reserved names; CheckAgentID does both.
func IsValidAgentID(id string) bool {
	return agentIDRe.MatchString(id)
}

// CheckAgentID applies every rule a brand-new agent id must pass: the format
// (IsValidAgentID) and not a reserved directory name under the install root
// (reservedAgentIDs, the names Validate refuses at load). It returns nil when
// id may be used.
func CheckAgentID(id string) error {
	if !IsValidAgentID(id) {
		return fmt.Errorf("agent id %q: must be a lowercase slug (^[a-z][a-z0-9-]*$)", id)
	}
	if reservedAgentIDs[id] {
		return fmt.Errorf("agent id %q: conflicts with reserved directory ~/%s/ — choose a different id", id, id)
	}
	return nil
}

// AgentMemorySourcePrefix heads the name under which an agent's own memory
// source is indexed: "agent/<source name>" (the default source's name is the
// agent id). Shared by the gateway's source setup (buildAgentMemorySources)
// and the agent-rename command, which renames the default source's rows.
const AgentMemorySourcePrefix = "agent/"
