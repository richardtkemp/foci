package agent

// HookList is a typed, append-only list of callbacks.
// Register with Add during setup; iterate with range during execution.
type HookList[F any] []F

// Add appends a callback to the hook list.
func (h *HookList[F]) Add(fn F) { *h = append(*h, fn) }
