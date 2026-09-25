package codex

import (
	"fmt"
	"strings"
)

// tomlBasicString renders s as a quoted TOML basic string for app-server
// configuration overrides.
func tomlBasicString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
