package main

import (
	"context"
	"strings"
	"time"

	"foci/anthropic"
	"foci/log"
	oai "foci/openai"
)

// modelLister is an interface for listing models, enabling test mocking.
type modelLister interface {
	ListModels() ([]anthropic.ModelInfo, error)
}

// resolveAnthropicAliases queries the Anthropic API for available models and
// updates aliases (haiku, sonnet, opus) in-place with the latest dated model ID.
// On API failure, existing alias values are kept unchanged.
func resolveAnthropicAliases(client modelLister, aliases map[string]string) {
	if aliases == nil {
		return
	}

	// Only resolve Anthropic family aliases
	families := []string{"haiku", "sonnet", "opus"}
	var toResolve []string
	for _, f := range families {
		if _, ok := aliases[f]; ok {
			toResolve = append(toResolve, f)
		}
	}
	if len(toResolve) == 0 {
		return
	}

	models, err := client.ListModels()
	if err != nil {
		log.Warnf("model-discovery", "failed to list Anthropic models: %v (keeping defaults)", err)
		return
	}

	for _, family := range toResolve {
		var bestID string
		var bestTime time.Time
		for _, m := range models {
			if !strings.Contains(strings.ToLower(m.ID), family) {
				continue
			}
			if m.CreatedAt.After(bestTime) {
				bestTime = m.CreatedAt
				bestID = m.ID
			}
		}
		if bestID != "" {
			old := aliases[family]
			aliases[family] = "anthropic/" + bestID
			log.Infof("model-discovery", "resolved %s → %s (was %s)", family, aliases[family], old)
		}
	}
}

// openaiModelLister is an interface for listing OpenAI models, enabling test mocking.
type openaiModelLister interface {
	ListModels(ctx context.Context) ([]oai.ModelInfo, error)
}

// openaiAliasFamily maps an alias key to the substring that should appear in
// matching model IDs. For example, alias "gpt4o" matches models containing "gpt-4o".
var openaiAliasFamilies = map[string]string{
	"gpt4o":  "gpt-4o",
	"o3":     "o3",
	"o4mini": "o4-mini",
}

// resolveOpenAIAliases queries the OpenAI API for available models and updates
// aliases (gpt4o, o3, o4mini) in-place with the latest model ID.
// On API failure, existing alias values are kept unchanged.
func resolveOpenAIAliases(ctx context.Context, client openaiModelLister, aliases map[string]string) {
	if aliases == nil {
		return
	}

	// Only resolve OpenAI family aliases that are present in the map
	type aliasEntry struct {
		key   string
		match string
	}
	var toResolve []aliasEntry
	for alias, match := range openaiAliasFamilies {
		if v, ok := aliases[alias]; ok && strings.HasPrefix(v, "openai/") {
			toResolve = append(toResolve, aliasEntry{key: alias, match: match})
		}
	}
	if len(toResolve) == 0 {
		return
	}

	models, err := client.ListModels(ctx)
	if err != nil {
		log.Warnf("model-discovery", "failed to list OpenAI models: %v (keeping defaults)", err)
		return
	}

	for _, entry := range toResolve {
		var bestID string
		var bestCreated int64
		for _, m := range models {
			if !strings.Contains(strings.ToLower(m.ID), entry.match) {
				continue
			}
			if m.Created > bestCreated {
				bestCreated = m.Created
				bestID = m.ID
			}
		}
		if bestID != "" {
			old := aliases[entry.key]
			aliases[entry.key] = "openai/" + bestID
			log.Infof("model-discovery", "resolved %s → %s (was %s)", entry.key, aliases[entry.key], old)
		}
	}
}
