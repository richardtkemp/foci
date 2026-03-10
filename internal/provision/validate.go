package provision

import "regexp"

var agentIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// IsValidAgentID checks if a string is a valid agent identifier (lowercase slug).
func IsValidAgentID(id string) bool {
	return agentIDRe.MatchString(id)
}
