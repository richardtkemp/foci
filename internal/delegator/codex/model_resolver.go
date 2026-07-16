package codex

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"foci/internal/delegator"
	"foci/internal/modelcaps"
)

// ResolveModel resolves an exact Codex model id or a case-insensitive
// substring alias against the live catalogue. Exact ids always win. Ambiguous
// substring matches prefer the numerically newest model version; catalogue
// order breaks ties because app-server presents its preferred models first.
func (b *Backend) ResolveModel(_ context.Context, model string) (delegator.ModelResolution, error) {
	b.mu.Lock()
	models := append([]string(nil), b.catalogueModels...)
	b.mu.Unlock()
	if len(models) == 0 {
		models = modelcaps.ModelsFor(modelcaps.BackendCodex)
	}
	resolved, err := resolveCatalogueModel(model, models)
	if err != nil {
		return delegator.ModelResolution{}, err
	}
	return delegator.ModelResolution{
		BackendModel: resolved,
		Model:        "codex/" + resolved,
	}, nil
}

func resolveCatalogueModel(model string, catalogue []string) (string, error) {
	query := strings.TrimSpace(model)
	if len(query) > len("codex/") && strings.EqualFold(query[:len("codex/")], "codex/") {
		query = query[len("codex/"):]
	}
	if query == "" {
		return "", fmt.Errorf("codex: model name is empty")
	}

	for _, candidate := range catalogue {
		if strings.EqualFold(candidate, query) {
			return candidate, nil
		}
	}

	query = strings.ToLower(query)
	best := ""
	for _, candidate := range catalogue {
		if !strings.Contains(strings.ToLower(candidate), query) {
			continue
		}
		if best == "" || compareModelVersions(candidate, best) > 0 {
			best = candidate
		}
	}
	if best == "" {
		return "", fmt.Errorf("codex: no catalogue model matches %q", model)
	}
	return best, nil
}

// compareModelVersions compares every numeric run in two ids lexicographically
// (5.12 > 5.9, and 5.6.1 > 5.6). Non-numeric text deliberately does not break
// ties, preserving the app-server's catalogue order.
func compareModelVersions(a, b string) int {
	av := modelVersionParts(a)
	bv := modelVersionParts(b)
	n := len(av)
	if len(bv) > n {
		n = len(bv)
	}
	for i := 0; i < n; i++ {
		var ai, bi uint64
		if i < len(av) {
			ai = av[i]
		}
		if i < len(bv) {
			bi = bv[i]
		}
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return 0
}

func modelVersionParts(model string) []uint64 {
	var parts []uint64
	for i := 0; i < len(model); {
		if model[i] < '0' || model[i] > '9' {
			i++
			continue
		}
		start := i
		for i < len(model) && model[i] >= '0' && model[i] <= '9' {
			i++
		}
		part, err := strconv.ParseUint(model[start:i], 10, 64)
		if err != nil {
			part = ^uint64(0)
		}
		parts = append(parts, part)
	}
	return parts
}
