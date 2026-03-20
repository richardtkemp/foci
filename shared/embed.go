// Package shared embeds default resource files shipped with foci.
// Character files, openclaw files, and the crontab template are embedded
// at build time so the binary is self-sufficient without a repo checkout.
//
// Skills and prompts have their own embed packages (shared/skills, shared/prompts).
package shared

import "embed"

//go:embed character/*.md openclaw/*.md crontab.template
var DefaultsFS embed.FS
