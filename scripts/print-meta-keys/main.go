// Command print-meta-keys prints every session_metadata key registered in
// internal/session's SessionMetadataKeys, one key per line, in registry
// order. It is the bridge that lets shell tooling consume the ONE registry
// without a copied key list: scripts/audit-session-metadata.sh parses this
// output line by line (main_test.go pins it to the registry).
//
// Usage: go run ./scripts/print-meta-keys   (from the repo root)
//
// Exit codes: 0 = keys printed, 1 = stdout write error.
package main

import (
	"fmt"
	"io"
	"os"

	"foci/internal/session"
)

func main() {
	if err := writeKeys(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "print-meta-keys: %v\n", err)
		os.Exit(1)
	}
}

// writeKeys writes each registry key, in registry order, one per line, with
// no header and no decoration — the exact output contract
// audit-session-metadata.sh parses.
func writeKeys(w io.Writer) error {
	for _, mk := range session.SessionMetadataKeys {
		if _, err := fmt.Fprintln(w, mk.Key); err != nil {
			return err
		}
	}
	return nil
}
