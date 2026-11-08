package config

import "sort"

// TTSOrder returns the [[tts]] entry ids in the order a speech request tries
// them (#2974, Dick 2026-11-08: "each voice defined in config gets a
// priority, highest priority is used first"). The ids in prefer come first, in
// their own order (an agent's voice.tts and voice.tts_order); every other entry
// follows by Priority, highest first, equal priorities in config order. An id
// in prefer that names no entry is left out, and each id appears once.
func TTSOrder(entries []TTSConfig, prefer []string) []string {
	known := make(map[string]bool, len(entries))
	var rest []TTSConfig
	for _, e := range entries {
		if known[e.ID] {
			continue
		}
		known[e.ID] = true
		rest = append(rest, e)
	}
	out := make([]string, 0, len(rest))
	placed := make(map[string]bool, len(rest))
	for _, id := range prefer {
		if known[id] && !placed[id] {
			placed[id] = true
			out = append(out, id)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Priority > rest[j].Priority })
	for _, e := range rest {
		if !placed[e.ID] {
			out = append(out, e.ID)
		}
	}
	return out
}
