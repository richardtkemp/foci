package app

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/fap"
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

// applyConfigEdit validates a put (value non-nil) or unset (value nil) and
// performs the file edit, routing by section kind: mapObject sections
// (models, endpoints) to applyMapObjectEdit, object lists
// (message_transforms, ...) to applyObjectListEdit, and scalar/map registry
// fields to the SetInFile/UnsetInFile planners. Every write goes through
// checkedFileEdit, which runs the whole-file load check before anything is
// written.
func (h *Hub) applyConfigEdit(scope, section, key string, value *string) error {
	// Map-object sections first, before the generic scope check, so a scoped
	// put is refused with the real reason (no per-agent override exists for
	// models/endpoints) rather than the generic scope/section mismatch.
	if spec, isMO := config.MapObjectFieldSpecFor(section); isMO {
		return h.applyMapObjectEdit(scope, spec, key, value)
	}
	if scope != "" && section != "agent" {
		return fmt.Errorf("agent scope %q requires the agent section, got [%s]", scope, section)
	}
	if scope == "" && section == "agent" {
		return fmt.Errorf("the agent section needs an agent scope")
	}
	// Object-list (array-of-tables) sections — message_transforms, blocked_paths,
	// memory.sources — are addressed with an empty key and carry the WHOLE list as
	// a JSON array-of-objects; there is no per-element set path. Route them to
	// the table-array planner, which replaces every [[section]] block atomically.
	if spec, isObj := config.ObjectFieldSpecFor(section); isObj && key == "" {
		return h.applyObjectListEdit(scope, spec, value)
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

	if value == nil {
		if err := h.checkedFileEdit(func(cur string) (string, error) {
			next, _, err := config.PlanUnsetInFile(cur, target)
			return next, err
		}); err != nil {
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
	if err := h.checkedFileEdit(func(cur string) (string, error) {
		next, _, err := config.PlanSetInFile(cur, target, formatted)
		return next, err
	}); err != nil {
		return err
	}
	h.applyLive(section, key)
	return nil
}

// checkedFileEdit is the ONE funnel every config-editor file write goes
// through: it serialises concurrent editor frames (configEditMu), reads the
// file once, runs the caller's pure plan to compute the post-edit content,
// refuses the edit when the whole-file load check fails (checkFileEdit — the
// next restart's verdict on the result), and only then writes atomically
// (config.WriteFileAtomic: temp file + rename, so no reader ever sees a
// half-written file). Nothing is written unless every check passes.
func (h *Hub) checkedFileEdit(plan func(cur string) (string, error)) error {
	h.configEditMu.Lock()
	defer h.configEditMu.Unlock()
	cfg := h.deps.Config
	data, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cur := string(data)
	next, err := plan(cur)
	if err != nil {
		return err
	}
	if err := h.checkFileEdit(cur, next); err != nil {
		return err
	}
	mode, err := config.ParseFileMode(cfg.FileMode)
	if err != nil {
		mode = 0o600
	}
	return config.WriteFileAtomic(cfg.SourcePath, next, mode)
}

// checkFileEdit is the editor's whole-file check: an edit is refused when its
// RESULT would stop the server from starting — the post-edit content is run
// through the same parse + Validate the next restart runs
// (config.CheckFileContent, with the registered backend names; unknown keys
// do not fail it, and like Load it touches no process-global state). A
// file that ALREADY fails the check stays editable — an operator must be
// able to repair a broken foci.toml through the editor rather than be locked
// out — so only a write that breaks a loadable file is refused; the error
// reply carries the load error verbatim. Validate's WARN lines re-logging on
// every check of a warning-carrying config is expected: this is deliberately
// the same Validate, with no quiet mode.
func (h *Hub) checkFileEdit(cur, next string) error {
	backends := delegator.RegisteredNames()
	if err := config.CheckFileContent(cur, h.deps.Config.SourcePath, backends); err != nil {
		return nil // already unloadable: stay editable
	}
	return config.CheckFileContent(next, h.deps.Config.SourcePath, backends)
}

// applyMapObjectEdit writes one mapObject-section entry edit ([models.*],
// [endpoints.*]): a put carries a JSON object of sub-field → display-form
// value with key "<entry>" (create or merge); an unset with key "<entry>"
// deletes the whole entry including its sub-tables, key "<entry>.<sub>"
// removes that one sub-field. Global scope only — there is no per-agent
// models/endpoints override surface. Like object lists, edits apply on
// restart: applyLive logs the uniform takes-effect-on-restart line (no
// applier is registered for these sections).
func (h *Hub) applyMapObjectEdit(scope string, spec config.MapObjectFieldSpec, key string, value *string) error {
	if scope != "" {
		return fmt.Errorf("mapObject section %q has no per-agent scope", spec.Section)
	}
	if value == nil {
		if err := h.checkedFileEdit(func(cur string) (string, error) {
			return config.PlanUnsetMapObject(cur, spec, key)
		}); err != nil {
			return err
		}
		h.applyLive(spec.Section, "")
		return nil
	}
	settings, err := config.ParseMapObjectValue(spec, *value)
	if err != nil {
		return err
	}
	if err := h.checkedFileEdit(func(cur string) (string, error) {
		return config.PlanSetMapObjectEntry(cur, spec, key, settings)
	}); err != nil {
		return err
	}
	h.applyLive(spec.Section, "")
	return nil
}

// applyObjectListEdit writes a whole array-of-tables section (value non-nil, a
// JSON array-of-objects) or clears it (value nil). Object-lists are global-only
// for now — per-agent [[agents.*]] overrides aren't surfaced by the editor yet.
// Most are restart-required; a section with a live applier ([[modelinfo]],
// ObjectFieldSpec.Live) is pushed into the process via applyLive, addressed by
// section with an empty key. Restart-required ones go through applyLive too,
// which logs that they take effect on restart.
func (h *Hub) applyObjectListEdit(scope string, spec config.ObjectFieldSpec, value *string) error {
	if scope != "" {
		return fmt.Errorf("object-list section %q has no per-agent scope yet", spec.Section)
	}
	var entries []map[string]any
	if value != nil {
		parsed, err := config.ParseObjectListValue(spec, *value)
		if err != nil {
			return err
		}
		entries = parsed
	}
	if err := h.checkedFileEdit(func(cur string) (string, error) {
		return config.PlanSetTableArray(cur, spec.Section, entries)
	}); err != nil {
		return err
	}
	h.applyLive(spec.Section, "")
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
// fresh parse of the FILE (truthful immediately after an edit); mapObject
// sections' entries are read from the decoded TOML tree
// (config.MapObjectFileEntries); an agent scope's values resolve through the
// ONE agent-value ladder (config.ResolveAgentValue): the file's inherited
// global-section value, then the running value (the mapped section's running
// global first, then the agent's own running row), then the built-in default.
// Per-field NeedsRestart records each field's intrinsic consumption pattern
// (see config.ConfigField.NeedsRestart): hot fields apply live via applyLive
// on edit, restart-required fields take effect on the next restart — the
// broadcast a client receives after every successful edit (a FAILED edit
// answers only the requester, with Error carrying the refusal: the field's
// own validation or the whole-file load check, see checkedFileEdit).
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

	// Object-list sections (message_transforms, blocked_paths, memory.sources,
	// modelinfo) are []struct array-of-tables, also outside the scalar registry.
	// Emit one descriptor each with type "object[]", carrying the entry sub-field
	// shapes in Fields; the section's current entries are a JSON array at
	// Values[section]. NeedsRestart is false when the section has a live applier
	// (of.Live), true otherwise.
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
			NeedsRestart: !of.Live,
			Fields:       sub,
		})
	}

	// Map-object sections (models, endpoints) are map[string]Struct — named
	// entries with typed sub-fields. Emit one descriptor each with type
	// "mapObject", carrying the entry sub-field shapes in Fields.
	for _, mo := range config.MapObjectFields() {
		sub := make([]fap.ConfigFieldDesc, 0, len(mo.Fields))
		for _, sf := range mo.Fields {
			sub = append(sub, fap.ConfigFieldDesc{
				Key:         sf.Key,
				ValueType:   sf.Type.TypeName(),
				Description: sf.Description,
			})
		}
		descs = append(descs, fap.ConfigFieldDesc{
			Section:      mo.Section,
			Key:          "",
			ValueType:    "mapObject",
			Description:  mo.Description,
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
	// Map-object sections (models, endpoints): current [section.*] entries as
	// a JSON object-of-objects, read from the decoded TOML tree so dotted
	// quoted entry names stay intact and sub-tables (e.g. [models.*.provider])
	// never leak in as sub-fields (config.MapObjectFileEntries).
	for _, mo := range config.MapObjectFields() {
		entries, err := config.MapObjectFileEntries(cfg.SourcePath, mo)
		if err != nil {
			continue
		}
		b, err := json.Marshal(entries)
		if err != nil {
			continue
		}
		gScope.Values[mo.Section] = string(b)
		if len(entries) > 0 {
			gScope.Explicit = append(gScope.Explicit, mo.Section)
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
			// The shared running rung (config.AgentRunningValue): the mapped
			// section's running global first, else this agent's row — override
			// fields like sessions.max_system_prompt_chars_file carry their
			// default only on the global struct, so the row alone would show "".
			v, _ := config.ResolveAgentValue(fileGlobal, fileAgents, a.ID, f, func() string {
				return config.AgentRunningValue(cfg, a, f.Key)
			})
			s.Values[full] = v
			if _, ok := av[f.Key]; ok {
				s.Explicit = append(s.Explicit, full)
			}
		}
		sort.Strings(s.Explicit)
		scopes = append(scopes, s)
	}

	return fap.ConfigSchema{Fields: descs, Scopes: scopes, Error: errMsg}
}
