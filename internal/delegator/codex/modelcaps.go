package codex

import (
	"encoding/json"
	"fmt"

	"foci/internal/modelcaps"
	"foci/internal/modelinfo"
)

// listModelCaps reads every page of Codex's model/list catalogue and converts
// it to foci's backend-neutral live capability shape. Codex supplies model ids
// and ordered reasoning-effort levels; structural details it omits are filled
// only from exact entries in foci's static model registry.
func (b *Backend) listModelCaps() (map[string]modelcaps.Caps, error) {
	entries := make(map[string]modelcaps.Caps)
	cursor := ""
	seenCursors := make(map[string]bool)

	for {
		result, err := b.sendAndWait("model/list", modelListParams{
			Cursor:        cursor,
			IncludeHidden: false,
		})
		if err != nil {
			return nil, err
		}

		var page modelListResponse
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, fmt.Errorf("codex: parse model/list response: %w", err)
		}
		for _, m := range page.Data {
			modelID := m.Model
			if modelID == "" {
				modelID = m.ID
			}
			if modelID == "" {
				continue
			}
			caps := modelcaps.Caps{}
			if static, ok := modelinfo.Lookup("codex", modelID); ok {
				caps.ContextWindow = static.ContextWindow
			}
			for _, option := range m.SupportedReasoningEfforts {
				if option.ReasoningEffort != "" {
					caps.Effort = append(caps.Effort, option.ReasoningEffort)
				}
			}
			entries[modelinfo.Normalize(modelID)] = caps
		}

		if page.NextCursor == nil || *page.NextCursor == "" {
			return entries, nil
		}
		if seenCursors[*page.NextCursor] {
			return nil, fmt.Errorf("codex: model/list repeated cursor %q", *page.NextCursor)
		}
		seenCursors[*page.NextCursor] = true
		cursor = *page.NextCursor
	}
}

// refreshModelCaps fetches and delivers one complete catalogue snapshot.
func (b *Backend) refreshModelCaps() error {
	entries, err := b.listModelCaps()
	if err != nil {
		return err
	}
	if b.onModelCaps != nil {
		b.onModelCaps(entries)
	}
	return nil
}
