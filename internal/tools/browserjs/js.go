// Package browserjs embeds the ARIA snapshot engine JavaScript.
// Vendored from go-rod/rod-mcp (MIT License).
package browserjs

import _ "embed"

//go:embed snapshotter.js
var SnapshotJS string

// AriaSnapshot is the JS expression that captures an accessibility tree snapshot.
// It must be called via page.Eval after injecting SnapshotJS.
const AriaSnapshot = `function(node, opts) { return snapshotEngine.ariaSnapshot(eval(node), eval(opts)); }`

// ScopedAriaSnapshot is the JS expression that captures a scoped accessibility
// tree snapshot rooted at a given element. Unlike AriaSnapshot, it does not
// overwrite _lastAriaSnapshot or stamp DOM elements — it reads existing stamps
// set by a prior full snapshot.
const ScopedAriaSnapshot = `function(node, opts) { return snapshotEngine.scopedAriaSnapshot(eval(node), eval(opts)); }`

// QueryEleByAria is the JS expression that finds an element by its snapshot ref.
// It must be called via page.ElementByJS after injecting SnapshotJS.
const QueryEleByAria = `(selector) => { return snapshotEngine.queryAll(selector); }`
