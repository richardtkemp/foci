package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"foci/internal/app/fap"
	"foci/internal/config"
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
	appLog.Infof("config set %s.%s (scope %q)", f.Section, f.Key, f.Scope)
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
	appLog.Infof("config unset %s.%s (scope %q)", f.Section, f.Key, f.Scope)
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
		appLog.Warnf("server.restart (device %q): no restart function wired", client.deviceID)
		return
	}
	if msg, err := h.deps.Restart(); err != nil {
		appLog.Errorf("server.restart (device %q): %v", client.deviceID, err)
	} else {
		appLog.Infof("server.restart (device %q): %s", client.deviceID, msg)
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
	// Object-list (array-of-tables) sections — message_transforms, blocked_paths,
	// memory.sources — are addressed with an empty key and carry the WHOLE list as
	// a JSON array-of-objects; there is no per-element set path. Route them to
	// SetTableArray, which replaces every [[section]] block atomically.
	if spec, isObj := config.ObjectFieldSpecFor(section); isObj && key == "" {
		return h.applyObjectListEdit(cfg, scope, spec, value)
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

// applyObjectListEdit writes a whole array-of-tables section (value non-nil, a
// JSON array-of-objects) or clears it (value nil). Object-lists are global-only
// for now — per-agent [[agents.*]] overrides aren't surfaced by the editor yet.
// They are restart-required (no live applier), so no applyLive call: the file
// write succeeds and the change takes effect on the next restart.
func (h *Hub) applyObjectListEdit(cfg *config.Config, scope string, spec config.ObjectFieldSpec, value *string) error {
	if scope != "" {
		return fmt.Errorf("object-list section %q has no per-agent scope yet", spec.Section)
	}
	mode, _ := config.ParseFileMode(cfg.FileMode)
	var entries []map[string]any
	if value != nil {
		parsed, err := config.ParseObjectListValue(spec, *value)
		if err != nil {
			return err
		}
		entries = parsed
	}
	if _, err := config.SetTableArray(cfg.SourcePath, spec.Section, entries, mode); err != nil {
		return err
	}
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
		appLog.Warnf("config %s.%s written but live apply failed (takes effect on restart): %v", section, key, err)
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

	// Object-list sections (message_transforms, blocked_paths, memory.sources) are
	// []struct array-of-tables, also outside the scalar registry. Emit one
	// descriptor each with type "object[]", carrying the entry sub-field shapes in
	// Fields; the section's current entries are a JSON array at Values[section].
	// Restart-required (no live applier) so NeedsRestart is set.
	for _, of := range config.ObjectFields() {
		sub := make([]fap.ConfigFieldDesc, 0, len(of.Fields))
		for _, sf := range of.Fields {
			sub = append(sub, fap.ConfigFieldDesc{
				Key:         sf.Key,
				ValueType:   sf.Type.TypeName(),
				Description: sf.Description,
			})
		}
		descs = append(descs, fap.ConfigFieldDesc{
			Section:      of.Section,
			Key:          "",
			ValueType:    "object[]",
			Description:  of.Description,
			NeedsRestart: true,
			Fields:       sub,
		})
	}

	fileGlobal, fileAgents, err := config.ExplicitFileValues(cfg.SourcePath)
	if err != nil {
		appLog.Warnf("config schema: parse %s: %v", cfg.SourcePath, err)
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
	// Object-list sections: current [[section]] blocks as a JSON array-of-objects.
	for _, of := range config.ObjectFields() {
		entries, err := config.TableArrayEntries(cfg.SourcePath, of)
		if err != nil {
			continue
		}
		b, err := json.Marshal(entries)
		if err != nil {
			continue
		}
		gScope.Values[of.Section] = string(b)
		if len(entries) > 0 {
			gScope.Explicit = append(gScope.Explicit, of.Section)
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
