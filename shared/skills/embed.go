// Package skills embeds the default skill files shipped with foci.
// Skills are seeded to ~/shared/skills/ on first run so users can customise them.
package skills

import "embed"

//go:embed all:*
var FS embed.FS
