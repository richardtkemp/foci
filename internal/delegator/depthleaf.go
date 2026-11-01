package delegator

// DepthLeafRefusal is the tool result a backend gives when a depth leaf tries
// to start subagent or background work by a path other than foci_spawn
// (#2625). path names what was refused ("The Agent tool", "A background
// shell command", ...).
//
// Why the rule exists (Dick, 2026-10-31): a fan-out completes only when every
// leaf has reported, so a leaf must not run long work that keeps the tree
// waiting. A foci_spawn is safe: when its leaf reports, its running spawns are
// reparented to the nearest open ancestor, so a leaf can start one and report
// at once. Every other path (CC's Agent tool, a background shell, codex and
// opencode subagents) reports only to the leaf's own backend process, so its
// result is lost once the leaf reports.
func DepthLeafRefusal(path string) string {
	return path + " is refused in a depth leaf. Subagent and background work in a depth leaf must go through " +
		"foci_spawn (any mode, --background included). A spawn's result is passed up the depth tree after this " +
		"leaf reports, so you may start a spawn and call foci_depth_report while it runs. Run shell commands in " +
		"the foreground."
}
