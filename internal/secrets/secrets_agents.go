package secrets

// Per-section agent rules (allowed_agents / denied_agents) as the root store
// edits them (#3090). A section may carry one list or the other, never both:
// parseBytes rejects a file with both, so each setter clears the other list
// in the same mutation, and the next Save can never write an unloadable file.

// SectionAllowedAgents returns the section's allowed_agents list (nil when it
// has none). It reads the global rule, whatever view s is.
func (s *Store) SectionAllowedAgents(section string) []string {
	return append([]string(nil), s.src.current().allowedAgents[section]...)
}

// SectionDeniedAgents returns the section's denied_agents list (nil when it
// has none). It reads the global rule, whatever view s is.
func (s *Store) SectionDeniedAgents(section string) []string {
	return append([]string(nil), s.src.current().deniedAgents[section]...)
}

// SetAllowedAgents makes agents the section's allowed_agents list and removes
// any denied_agents list. An empty agents removes both lists: the section is
// then visible to every agent. (An explicit allowed_agents = [] in the file
// denies every agent; this setter never writes one.)
func (s *Store) SetAllowedAgents(section string, agents []string) error {
	return s.setAgentRule(section, agents, true)
}

// SetDeniedAgents makes agents the section's denied_agents list and removes
// any allowed_agents list. An empty agents removes both lists.
func (s *Store) SetDeniedAgents(section string, agents []string) error {
	return s.setAgentRule(section, agents, false)
}

// setAgentRule is SetAllowedAgents (allow) or SetDeniedAgents.
func (s *Store) setAgentRule(section string, agents []string, allow bool) error {
	if err := s.rootOnly(); err != nil {
		return err
	}
	if err := s.notStatic(); err != nil {
		return err
	}
	s.src.mutate(func(st *fileState) *fileState {
		next := *st
		next.allowedAgents = cloneMap(st.allowedAgents)
		next.deniedAgents = cloneMap(st.deniedAgents)
		delete(next.allowedAgents, section)
		delete(next.deniedAgents, section)
		if len(agents) > 0 {
			list := append([]string(nil), agents...)
			if allow {
				next.allowedAgents[section] = list
			} else {
				next.deniedAgents[section] = list
			}
		}
		return &next
	})
	return nil
}
