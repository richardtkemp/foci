package provision

import "foci/internal/config"

// IsValidAgentID checks if a string is a valid agent identifier (lowercase slug).
// The rule lives in config (config.IsValidAgentID) so every id check shares it.
func IsValidAgentID(id string) bool {
	return config.IsValidAgentID(id)
}
