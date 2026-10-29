package tools

import "foci/internal/delegator"

// spawnToolNames are the spawn family. No one-shot gets them: a one-shot that
// could spawn would recurse with nobody watching (the API one-shots' tool
// sets skip spawn too, spawnToolSet).
var spawnToolNames = map[string]bool{
	"spawn":        true,
	"spawn_send":   true,
	"spawn_status": true,
}

// parentBatchExcluded lists the tools a parent-access batch session's exec
// bridge (spawn character) leaves out of the agent's own set: the spawn
// family (above); close_session, which on a batch session (keyed under its
// owner's root) would close the OWNER's chat on a throwaway session's say-so
// (#2573); and ask, since nobody is there to answer a character spawn
// (#2643: "anything that would need the user is refused").
var parentBatchExcluded = map[string]bool{
	"close_session": true,
	"ask":           true,
}

// Subset returns a registry holding only the tools keep accepts. The tools
// are shared, not copied.
func (r *Registry) Subset(keep func(name string) bool) *Registry {
	out := NewRegistry()
	for name, t := range r.tools {
		if keep(name) {
			out.Register(t)
		}
	}
	return out
}

// RegistryForAccess returns the tools a session with this batch access may
// reach through its exec bridge, or nil for none (no bridge at all):
//   - default (every non-batch session, and foci's own batches): the agent's
//     whole registry;
//   - parent: the agent's registry less the spawn family, close_session and ask;
//   - isolated: none — the session runs no commands, so it has no shell to
//     reach a bridge from, and the bridge's tools are not confined to its
//     directory;
//   - read-only: the explore allowlist (spawnExploreAllowed), the same tools
//     an API explore spawn gets from the registry.
func RegistryForAccess(reg *Registry, access delegator.BatchAccess) *Registry {
	if reg == nil {
		return nil
	}
	switch access {
	case delegator.BatchAccessDefault:
		return reg
	case delegator.BatchAccessParent:
		return reg.Subset(func(name string) bool { return !spawnToolNames[name] && !parentBatchExcluded[name] })
	case delegator.BatchAccessReadOnly:
		return reg.Subset(func(name string) bool { return spawnExploreAllowed[name] })
	}
	return nil
}
