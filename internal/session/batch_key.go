package session

import (
	"fmt"

	"foci/internal/timeutil"
)

// NewBatchKey returns the key of a fresh one-shot run done for owner (a
// delegated batch, an API character spawn): <owner's root>/b<unique ts>,
// and its parent (owner itself). The key must be well-formed: a key
// ParseSessionKey can't read indexes with chat_id=0/is_root=0, which no family
// query can reach. With no parseable owner the run hangs off a synthetic
// independent root, <agentID>/ibatch, which is then also its parent.
func NewBatchKey(owner, agentID string) (key, parent string) {
	root := SessionKey{AgentID: agentID, Type: 'i', ID: "batch"}.String()
	parent = root
	if sk, err := ParseSessionKey(owner); err == nil {
		root = sk.Root().String()
		parent = owner
	}
	// UniqueNano: concurrent runs must not share a key on a microsecond
	// clock (macOS).
	return fmt.Sprintf("%s/b%d", root, timeutil.UniqueNano()), parent
}
