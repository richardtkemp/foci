package opencode

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/procx"
)

// matchModel finds the unique model ID in lines that contains model as a
// substring. Returns the full model ID on a unique match, or an error if
// zero or more than one match is found.
func matchModel(model string, lines []string) (string, error) {
	var matches []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, model) {
			matches = append(matches, line)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("model %q not found in opencode models", model)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("model %q is ambiguous; matches: %s", model, strings.Join(matches, ", "))
	}
}

// resolveModel runs `opencode models` and verifies that model uniquely
// matches exactly one available model. Returns the full model ID (e.g.
// "zai-coding-plan/glm-5.2") on success.
func resolveModel(ctx context.Context, binaryPath, workDir, model string) (string, error) {
	if binaryPath == "" {
		binaryPath = "opencode"
	}
	cmd := procx.Spawn(ctx, binaryPath, "models")
	cmd.Dir = workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("opencode models: %w", err)
	}
	return matchModel(model, strings.Split(string(output), "\n"))
}
