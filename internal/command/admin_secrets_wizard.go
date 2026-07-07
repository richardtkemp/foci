package command

import (
	"encoding/json"
	"fmt"
	"strings"
)

// secretsSetWizard implements WizardHandler for interactive secret value entry.
// Steps: 0 = enter section.key, 1 = enter value, 2 = optional allowed_hosts.
type secretsSetWizard struct {
	step    int
	store   SecretsStore
	section string
	key     string
}

func newSecretsSetWizard(store SecretsStore) *secretsSetWizard {
	return &secretsSetWizard{store: store}
}

// wizardKindSecretsSet is the persisted-snapshot kind tag for secretsSetWizard.
// Only the step + section/key persist — never a secret value (values are
// written straight to the store as they're entered, not held on the wizard).
const wizardKindSecretsSet = "secrets-set"

type secretsSetWizardSnapshot struct {
	Step    int    `json:"step"`
	Section string `json:"section,omitempty"`
	Key     string `json:"key,omitempty"`
}

func (w *secretsSetWizard) WizardKind() string { return wizardKindSecretsSet }

func (w *secretsSetWizard) SnapshotWizard() ([]byte, error) {
	return json.Marshal(secretsSetWizardSnapshot{Step: w.step, Section: w.section, Key: w.key})
}

func (w *secretsSetWizard) RestoreWizard(data []byte) error {
	var s secretsSetWizardSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	w.step, w.section, w.key = s.Step, s.Section, s.Key
	return nil
}

// Handle processes a wizard step and returns the response.
func (w *secretsSetWizard) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)

	switch w.step {
	case 0:
		return w.handleName(text)
	case 1:
		return w.handleValue(text)
	case 2:
		return w.handleHosts(text)
	default:
		return "Unexpected state.", true
	}
}

func (w *secretsSetWizard) handleName(text string) (string, bool) {
	if text == "" {
		return "Name cannot be empty. Enter section.key (e.g. custom.api_key):", false
	}
	if !strings.Contains(text, ".") {
		return "Key must be in section.key format (e.g. custom.api_key). Try again:", false
	}

	sec, key, _ := strings.Cut(text, ".")
	w.section = sec
	w.key = key
	w.step = 1

	hosts := w.store.SectionAllowedHosts(sec)
	hostsStr := "(none)"
	if len(hosts) > 0 {
		hostsStr = strings.Join(hosts, ", ")
	}
	return fmt.Sprintf("Set value for %s.%s\nAllowed hosts for [%s]: %s\n\nEnter value (or /stop to cancel):", sec, key, sec, hostsStr), false
}

func (w *secretsSetWizard) handleValue(text string) (string, bool) {
	if text == "" {
		return "Value cannot be empty. Try again:", false
	}

	w.store.Set(w.section+"."+w.key, text)
	if err := w.store.Save(); err != nil {
		return fmt.Sprintf("Failed to save: %s", err), true
	}

	w.step = 2

	hosts := w.store.SectionAllowedHosts(w.section)
	current := "(none)"
	if len(hosts) > 0 {
		current = strings.Join(hosts, ", ")
	}
	return fmt.Sprintf("Secret %s.%s set.\n\nSet allowed_hosts for [%s]? (current: %s)\nEnter hosts comma-separated, or /stop to skip:",
		w.section, w.key, w.section, current), false
}

func (w *secretsSetWizard) handleHosts(text string) (string, bool) {
	if text == "" {
		return "Enter hosts comma-separated, or /stop to skip:", false
	}

	// Parse comma-separated hosts.
	var hosts []string
	for _, h := range strings.Split(text, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			hosts = append(hosts, h)
		}
	}

	if len(hosts) == 0 {
		return "No valid hosts provided. Enter hosts comma-separated, or /stop to skip:", false
	}

	w.store.SetAllowedHosts(w.section, hosts)
	if err := w.store.Save(); err != nil {
		return fmt.Sprintf("Failed to save hosts: %s", err), true
	}

	return fmt.Sprintf("Set allowed_hosts for [%s]: %s", w.section, strings.Join(hosts, ", ")), true
}

// secretsHostsAddWizard implements WizardHandler for adding a single allowed host.
type secretsHostsAddWizard struct {
	store   SecretsStore
	section string
}

func newSecretsHostsAddWizard(store SecretsStore, section string) *secretsHostsAddWizard {
	return &secretsHostsAddWizard{store: store, section: section}
}

// wizardKindSecretsHostsAdd is the persisted-snapshot kind tag for secretsHostsAddWizard.
const wizardKindSecretsHostsAdd = "secrets-hosts-add"

type secretsHostsAddSnapshot struct {
	Section string `json:"section"`
}

func (w *secretsHostsAddWizard) WizardKind() string { return wizardKindSecretsHostsAdd }

func (w *secretsHostsAddWizard) SnapshotWizard() ([]byte, error) {
	return json.Marshal(secretsHostsAddSnapshot{Section: w.section})
}

func (w *secretsHostsAddWizard) RestoreWizard(data []byte) error {
	var s secretsHostsAddSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	w.section = s.Section
	return nil
}

// Handle takes user input as a host and adds it.
func (w *secretsHostsAddWizard) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "Host cannot be empty. Enter host (or /stop to cancel):", false
	}

	host := strings.ToLower(text)
	w.store.AddAllowedHost(w.section, host)
	if err := w.store.Save(); err != nil {
		return fmt.Sprintf("Failed to save: %s", err), true
	}

	return fmt.Sprintf("Added %s to [%s] allowed_hosts.", host, w.section), true
}
