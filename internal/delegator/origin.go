package delegator

import "fmt"

// RedirectedOrigin is the leading line of anything shown in a chat that is not
// its source's own: "↪️ **From <source>** (<why>)". The gateway uses it for a
// backend prompt offered to an ancestor's or the default chat because the
// asking session's chat cannot show it (#2608); the codex backend uses it for
// output from an app-server thread that matches no session and no known
// subagent, handed to the app-server's owner. One helper, so every redirected
// message reads the same way.
func RedirectedOrigin(source, why string) string {
	return fmt.Sprintf("↪️ **From %s** (%s)", source, why)
}
