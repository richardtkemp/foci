package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"foci/internal/delegator"
	"foci/internal/procx"
	"foci/internal/tempdir"
)

// RunBatch implements delegator.BatchRunner: a one-shot `codex exec`
// invocation on the agent's codex auth. Runs on an unstarted Backend
// instance — only cfg (binary override) is consulted.
//
// --ephemeral skips session-file persistence; --skip-git-repo-check allows
// non-repo workdirs (agent workspaces usually are repos, but batch runs must
// not depend on it). The system prompt is replaced via `-c instructions=…`
// (verified against codex v0.144.5: replaces base instructions the same way
// the app-server's threadStartParams.baseInstructions does). The final
// message is read from --output-last-message, which is exact — stdout mixes
// progress with the reply.
func (b *Backend) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	out, err := tempdir.Create("codex-batch-*.txt")
	if err != nil {
		return "", fmt.Errorf("codex batch: temp file: %w", err)
	}
	outPath := out.Name()
	_ = out.Close()
	defer func() { _ = os.Remove(outPath) }()

	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--color", "never",
		"--output-last-message", outPath,
	}
	if req.SystemPrompt != "" {
		args = append(args, "-c", "instructions="+tomlBasicString(req.SystemPrompt))
	}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.WorkDir != "" {
		args = append(args, "-C", req.WorkDir)
	}
	args = append(args, "-") // read the prompt from stdin (it can be large)

	cmd := procx.Spawn(ctx, b.codexBinary(), args...)
	cmd.Stdin = strings.NewReader(req.Prompt)

	var stderr bytes.Buffer
	cmd.Stdout = nil // progress noise; the reply comes from outPath
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", fmt.Errorf("codex exec failed: %w (stderr: %s)", err, s)
		}
		return "", fmt.Errorf("codex exec failed: %w", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("codex batch: read last message: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// tomlBasicString renders s as a quoted TOML basic string (escaping
// backslash, double quote, and control characters), so a multi-line system
// prompt survives `-c instructions=<value>` — codex parses the value portion
// as TOML. Relying on codex's "raw string on TOML parse failure" fallback is
// fragile for arbitrary text (a value can accidentally parse as something
// else), so always produce valid TOML.
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
