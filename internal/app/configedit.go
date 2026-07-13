package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"foci/internal/app/fap"
	"foci/internal/config"
	"foci/internal/log"
)

// featureConfigEdit is the ClientHello capability a client advertises to view
// and edit the server's own configuration (foci.toml) over config.* frames
// (wire §13). The server advertises it in hello.caps when a config file path
// is known (i.e. the config was loaded from disk).
const featureConfigEdit = "configEdit"

// configEditAvailable reports whether this hub can serve config.* frames.
func (h *Hub) configEditAvailable() bool {
	return h.deps.Config != nil && h.deps.Config.SourcePath != ""
}

// handleConfigGet answers one client's schema request.
func (h *Hub) handleConfigGet(client *wsClient) {
	if !h.configEditAvailable() {
		return
	}
	client.sendRaw(h.buildConfigSchema(""))
}

// handleConfigPut validates and writes one key to foci.toml, then broadcasts
// the fresh state to every configEdit-capable client. A failed edit answers
// only the requester, with Error set and values unchanged.
func (h *Hub) handleConfigPut(client *wsClient, f fap.ConfigPut) {
	if !h.configEditAvailable() {
		return
	}
	if err := h.applyConfigEdit(f.Scope, f.Section, f.Key, &f.Value); err != nil {
		client.sendRaw(h.buildConfigSchema(err.Error()))
		return
	}
	log.Infof("app", "config set %s.%s (scope %q)", f.Section, f.Key, f.Scope)
	h.broadcastConfigSchema()
}

// handleConfigUnset removes one explicitly-set key (revert to inherited/default).
func (h *Hub) handleConfigUnset(client *wsClient, f fap.ConfigUnset) {
	if !h.configEditAvailable() {
		return
	}
	if err := h.applyConfigEdit(f.Scope, f.Section, f.Key, nil); err != nil {
		client.sendRaw(h.buildConfigSchema(err.Error()))
		return
	}
	log.Infof("app", "config unset %s.%s (scope %q)", f.Section, f.Key, f.Scope)
	h.broadcastConfigSchema()
}

// handleServerRestart restarts the whole server on request from a config-editing
// client (the /restart path). Gated on the same configEdit capability as the
// config put/unset handlers — a client allowed to change restart-only settings
// is the one that needs to apply them. The restart drops every socket (clients
// see close code 1012 and fast-reconnect), so there is no reply to send.
func (h *Hub) handleServerRestart(client *wsClient) {
	if !h.configEditAvailable() {
		return
	}
	if h.deps.Restart == nil {
		log.Warnf("app", "server.restart (device %q): no restart function wired", client.deviceID)
		return
	}
	if msg, err := h.deps.Restart(); err != nil {
		log.Errorf("app", "server.restart (device %q): %v", client.deviceID, err)
	} else {
		log.Infof("app", "server.restart (device %q): %s", client.deviceID, msg)
	}
}

// applyConfigEdit validates a put (value non-nil) or unset (value nil) against
// the field registry and performs the file edit.
func (h *Hub) applyConfigEdit(scope, section, key string, value *string) error {
	cfg := h.deps.Config
	if scope != "" && section != "agent" {
		return fmt.Errorf("agent scope %q requires the agent section, got [%s]", scope, section)
	}
	if scope == "" && section == "agent" {
		return fmt.Errorf("the agent section needs an agent scope")
	}
	field, ok := config.LookupField(section + "." + key)
	if !ok {
		return fmt.Errorf("unknown config field %s.%s", section, key)
	}

	target := config.SetTarget{Section: section, Key: key}
	if scope != "" {
		if !h.agentExists(scope) {
			return fmt.Errorf("unknown agent %q", scope)
		}
		target = config.SetTarget{Section: "agents", AgentID: scope, Key: key}
	}

	mode, _ := config.ParseFileMode(cfg.FileMode)
	if value == nil {
		if _, err := config.UnsetInFile(cfg.SourcePath, target, mode); err != nil {
			return err
		}
		h.applyLive(section, key)
		return nil
	}
	if err := field.ValidateValue(*value); err != nil {
		return err
	}
	formatted, err := config.FormatTOMLValue(*value, field.Type)
	if err != nil {
		return err
	}
	if _, err := config.SetInFile(cfg.SourcePath, target, formatted, mode); err != nil {
		return err
	}
	h.applyLive(section, key)
	return nil
}

// applyLive pushes a hot field's just-written value into the running process.
// A failure never fails the edit — the file write already succeeded and the
// value applies on restart like any restart-required field.
func (h *Hub) applyLive(section, key string) {
	if h.deps.ApplyLive == nil {
		return
	}
	if _, err := h.deps.ApplyLive(section, key); err != nil {
		log.Warnf("app", "config %s.%s written but live apply failed (takes effect on restart): %v", section, key, err)
	}
}

func (h *Hub) agentExists(id string) bool {
	for _, a := range h.deps.Config.Agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// broadcastConfigSchema fans the current config state out to every
// configEdit-capable client, so an edit on one device reconciles on the others.
func (h *Hub) broadcastConfigSchema() {
	schema := h.buildConfigSchema("")
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.mu.Lock()
		_, ok := c.features[featureConfigEdit]
		c.mu.Unlock()
		if ok {
			c.sendRaw(schema)
		}
	}
}

// buildConfigSchema assembles the wire §13 payload: the field registry plus
// one scope per editing surface. Explicitness and explicit values come from a
// fresh parse of the FILE (truthful immediately after an edit); non-explicit
// values fall back to the inherited chain — for an agent scope the global
// value (file, then running config), then the built-in default. Per-field
// NeedsRestart records each field's intrinsic consumption pattern (see
// config.ConfigField.NeedsRestart): hot fields apply live via applyLive on
// edit, restart-required fields take effect on the next restart.
func (h *Hub) buildConfigSchema(errMsg string) fap.ConfigSchema {
	cfg := h.deps.Config
	fields := config.AllFields()

	agentToGlobalMap := config.AgentGlobalSections()
	// globalExists guards GlobalKey: only emit an address the registry really has.
	globalExists := map[string]bool{}
	for _, f := range fields {
		if f.Section != "agent" {
			globalExists[f.Section+"."+f.Key] = true
		}
	}

	descs := make([]fap.ConfigFieldDesc, 0, len(fields))
	for _, f := range fields {
		d := fap.ConfigFieldDesc{
			Section:      f.Section,
			Key:          f.Key,
			ValueType:    f.Type.TypeName(),
			Description:  f.Description,
			Default:      f.Default,
			NeedsRestart: f.NeedsRestart,
		}
		if c := f.GetConstraint(); c != nil {
			d.Min = c.Min
			d.Max = c.Max
			d.Choices = c.Choices
		}
		if f.Section == "agent" {
			if prefix, rest, ok := strings.Cut(f.Key, "."); ok {
				if gs, mapped := agentToGlobalMap[prefix]; mapped && globalExists[gs+"."+rest] {
					d.GlobalKey = gs + "." + rest
				}
			}
		}
		descs = append(descs, d)
	}

	// Map sections (groups, groups.calls, groups.fallbacks, system.webhooks) are
	// dynamic-key maps not in the scalar registry. Emit one descriptor each with
	// type "map"; each scope carries the section's current entries as a JSON
	// object at Values[section]. Global scope only for now — per-agent map
	// overrides are addressed but not yet surfaced here.
	for _, mf := range config.MapFields() {
		descs = append(descs, fap.ConfigFieldDesc{
			Section:     mf.Section,
			Key:         "",
			ValueType:   "map",
			Description: mf.Description,
		})
	}

	fileGlobal, fileAgents, err := config.ExplicitFileValues(cfg.SourcePath)
	if err != nil {
		log.Warnf("app", "config schema: parse %s: %v", cfg.SourcePath, err)
		fileGlobal, fileAgents = map[string]string{}, map[string]map[string]string{}
		if errMsg == "" {
			errMsg = fmt.Sprintf("config file unreadable: %v", err)
		}
	}
	agentToGlobal := agentToGlobalMap

	// globalValue resolves a global-section field: file value, else the
	// running config's value, else the built-in default from the struct tag.
	globalValue := func(section, key string) (string, bool) {
		full := section + "." + key
		if v, ok := fileGlobal[full]; ok {
			return v, true
		}
		if v := config.LookupValue(cfg, config.AgentConfig{}, section, key); v != "" {
			return v, false
		}
		return "", false
	}

	gScope := fap.ConfigScope{ID: "", Label: "Global", Values: map[string]string{}, Explicit: []string{}}
	for _, f := range fields {
		if f.Section == "agent" {
			continue
		}
		full := f.Section + "." + f.Key
		v, explicit := globalValue(f.Section, f.Key)
		if v == "" {
			v = f.Default
		}
		gScope.Values[full] = v
		if explicit {
			gScope.Explicit = append(gScope.Explicit, full)
		}
	}
	// Map sections: current entries as a JSON object, keyed by the map section.
	for _, mf := range config.MapFields() {
		entries := config.MapEntries(mf.Section, fileGlobal)
		b, err := json.Marshal(entries)
		if err != nil {
			continue
		}
		gScope.Values[mf.Section] = string(b)
		if len(entries) > 0 {
			gScope.Explicit = append(gScope.Explicit, mf.Section)
		}
	}
	sort.Strings(gScope.Explicit)
	scopes := []fap.ConfigScope{gScope}

	for _, a := range cfg.Agents {
		s := fap.ConfigScope{ID: a.ID, Label: a.Name, Values: map[string]string{}, Explicit: []string{}}
		if s.Label == "" {
			s.Label = a.ID
		}
		av := fileAgents[a.ID]
		for _, f := range fields {
			if f.Section != "agent" {
				continue
			}
			full := "agent." + f.Key
			if v, ok := av[f.Key]; ok {
				s.Values[full] = v
				s.Explicit = append(s.Explicit, full)
				continue
			}
			// Inherited: the matching global section's value (the agent key's
			// first segment names the AgentConfig sub-struct; AgentGlobalSections
			// maps it to the global registry section), else the running merged
			// value, else the built-in default.
			v := ""
			if prefix, rest, ok := strings.Cut(f.Key, "."); ok {
				if gs, mapped := agentToGlobal[prefix]; mapped {
					v, _ = globalValue(gs, rest)
				}
			}
			if v == "" {
				v = config.LookupValue(cfg, a, "agent", f.Key)
			}
			if v == "" {
				v = f.Default
			}
			s.Values[full] = v
		}
		sort.Strings(s.Explicit)
		scopes = append(scopes, s)
	}

	return fap.ConfigSchema{Fields: descs, Scopes: scopes, Error: errMsg}
}
