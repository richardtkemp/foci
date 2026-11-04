package secrets

import (
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"foci/internal/log"
)

// captureWarnings collects the secrets package's WARN messages for the test.
func captureWarnings(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	remove := log.AddTap(log.WARN, func(_ log.Level, component, msg string) {
		if component == "secrets" {
			mu.Lock()
			defer mu.Unlock()
			msgs = append(msgs, msg)
		}
	})
	t.Cleanup(remove)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(msgs)
	}
}

// assertOnlySecretListed proves a metadata value is not a secret: the store
// lists and masks only the real secret, and the metadata text survives Redact.
func assertOnlySecretListed(t *testing.T, s *Store, secretName, secretValue string, metadataText ...string) {
	t.Helper()
	if got := s.Names(); !slices.Equal(got, []string{secretName}) {
		t.Errorf("Names = %v, want only %q", got, secretName)
	}
	if vals := s.RedactionValues(); !slices.Equal(vals, []string{secretValue}) {
		t.Errorf("RedactionValues has %d values, want only the secret", len(vals))
	}
	if got := s.Redact("key " + secretValue); got != "key [REDACTED]" {
		t.Errorf("the secret was not masked: %q", got)
	}
	for _, text := range metadataText {
		if got := s.Redact(text); got != text {
			t.Errorf("metadata text was masked: %q became %q", text, got)
		}
	}
}

// TestMetadataKeys_GlobalForms runs every form an operator can write through
// the same assertions: redaction, listed names and the enforced lists (#2866).
func TestMetadataKeys_GlobalForms(t *testing.T) {
	for _, tc := range []struct {
		name, hosts, agents, body string
	}{
		{"arrays", `["api.example.com", "api.backup.example.com"]`, `["scout", "researcher"]`, `["token"]`},
		{"strings", `"api.example.com, api.backup.example.com"`, `"scout,researcher"`, `"token"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Load(writeSecrets(t, `
[svc]
token = "sk-the-real-secret"
allowed_hosts = `+tc.hosts+`
allowed_agents = `+tc.agents+`
allowed_in_body = `+tc.body+`
`))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertOnlySecretListed(t, s, "svc.token", "sk-the-real-secret",
				"api.example.com", "api.backup.example.com", "scout", "researcher")

			if got := s.AllowedHosts("svc.token"); !slices.Equal(got, []string{"api.example.com", "api.backup.example.com"}) {
				t.Errorf("AllowedHosts = %v", got)
			}
			if err := s.CheckHostAllowed("svc.token", "https://api.backup.example.com/x"); err != nil {
				t.Errorf("CheckHostAllowed: %v", err)
			}
			if !s.IsAllowedInBody("svc.token") {
				t.Error("allowed_in_body was lost")
			}
			if _, ok := s.ForAgent("researcher").Get("svc.token"); !ok {
				t.Error("a listed agent lost access")
			}
			if _, ok := s.ForAgent("intruder").Get("svc.token"); ok {
				t.Error("allowed_agents did not restrict the section: an unlisted agent read the secret")
			}
			if !s.HasAgentRestrictions() {
				t.Error("HasAgentRestrictions = false")
			}
		})
	}
}

// TestMetadataKeys_DeniedAgentsForms proves denied_agents is enforced in both
// forms.
func TestMetadataKeys_DeniedAgentsForms(t *testing.T) {
	for _, form := range []string{`["temp-bot", "other-bot"]`, `"temp-bot, other-bot"`} {
		t.Run(form, func(t *testing.T) {
			s, err := Load(writeSecrets(t, "[svc]\ntoken = \"sk-the-real-secret\"\ndenied_agents = "+form+"\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertOnlySecretListed(t, s, "svc.token", "sk-the-real-secret", "temp-bot", "other-bot")
			if _, ok := s.ForAgent("other-bot").Get("svc.token"); ok {
				t.Error("denied_agents did not restrict the section: a denied agent read the secret")
			}
			if _, ok := s.ForAgent("scout").Get("svc.token"); !ok {
				t.Error("an agent that is not denied lost access")
			}
		})
	}
}

// TestMetadataKeys_AgentTableForms proves the same rule in [agents.<id>.*]
// tables: the lists are enforced, and an access-list key, which has no meaning
// there, is neither a secret nor silent.
func TestMetadataKeys_AgentTableForms(t *testing.T) {
	for _, tc := range []struct {
		name, hosts, body, agents string
	}{
		{"arrays", `["agent.example.com"]`, `["token"]`, `["scout"]`},
		{"strings", `"agent.example.com"`, `"token"`, `"scout"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureWarnings(t)
			s, err := Load(writeSecrets(t, `
[agents.scout.svc]
token = "sk-the-real-secret"
allowed_hosts = `+tc.hosts+`
allowed_in_body = `+tc.body+`
allowed_agents = `+tc.agents+`
denied_agents = `+tc.agents+`
`))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			view := s.ForAgent("scout")
			assertOnlySecretListed(t, view, "svc.token", "sk-the-real-secret", "agent.example.com", "scout")
			if got := s.RedactionValues(); !slices.Equal(got, []string{"sk-the-real-secret"}) {
				t.Errorf("root RedactionValues has %d values, want only the secret", len(got))
			}
			if got := view.AllowedHosts("svc.token"); !slices.Equal(got, []string{"agent.example.com"}) {
				t.Errorf("AllowedHosts = %v", got)
			}
			if !view.IsAllowedInBody("svc.token") {
				t.Error("allowed_in_body was lost")
			}
			joined := strings.Join(warnings(), "\n")
			for _, key := range []string{"allowed_agents", "denied_agents"} {
				if !strings.Contains(joined, "agents.scout.svc") || !strings.Contains(joined, key) {
					t.Errorf("no warning names [agents.scout.svc] %s; got:\n%s", key, joined)
				}
			}
		})
	}
}

// TestMetadataKeys_StringFormWarns proves a string form is visible to the
// operator, and that the warning does not carry the value.
func TestMetadataKeys_StringFormWarns(t *testing.T) {
	warnings := captureWarnings(t)
	if _, err := Load(writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\nallowed_hosts = \"api.example.com,b.example.com\"\n")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(warnings(), "\n")
	if !strings.Contains(joined, "[svc]") || !strings.Contains(joined, "allowed_hosts") {
		t.Errorf("no warning names [svc] allowed_hosts; got:\n%s", joined)
	}
	if strings.Contains(joined, "api.example.com") {
		t.Errorf("the warning echoes the value:\n%s", joined)
	}
}

// TestMetadataKeys_ArrayFormDoesNotWarn is the control for the warning test.
func TestMetadataKeys_ArrayFormDoesNotWarn(t *testing.T) {
	warnings := captureWarnings(t)
	if _, err := Load(writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\nallowed_hosts = [\"api.example.com\"]\n")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := warnings(); len(got) != 0 {
		t.Errorf("a correct file warned: %v", got)
	}
}

// TestMetadataKeys_WrongTypeRejected proves a value that cannot be a list
// rejects the file, as a section with both access lists does. Before #2866 an
// integer became a secret, a boolean vanished and a mixed list lost elements:
// each of those could leave a section unrestricted with no warning.
func TestMetadataKeys_WrongTypeRejected(t *testing.T) {
	for _, tc := range []struct{ section, key, value string }{
		{"svc", "allowed_agents", "90125"},
		{"svc", "allowed_agents", "true"},
		{"svc", "denied_agents", "1.5"},
		{"svc", "denied_agents", `["temp-bot", 90125]`},
		{"svc", "allowed_hosts", "90125"},
		{"svc", "allowed_in_body", "2026-11-04"},
		{"agents.scout.svc", "allowed_hosts", "90125"},
		{"agents.scout.svc", "allowed_in_body", "true"},
	} {
		t.Run(tc.section+"/"+tc.key+"="+tc.value, func(t *testing.T) {
			_, err := Load(writeSecrets(t, "["+tc.section+"]\ntoken = \"sk-x-secret\"\n"+tc.key+" = "+tc.value+"\n"))
			if err == nil {
				t.Fatal("Load accepted a metadata key of the wrong type")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.section) || !strings.Contains(msg, tc.key) {
				t.Errorf("the error does not name the section and key: %q", msg)
			}
			if strings.Contains(msg, "90125") || strings.Contains(msg, "sk-x-secret") {
				t.Errorf("the error echoes file bytes: %q", msg)
			}
		})
	}
}

// TestMetadataKeys_WrongTypeOnReloadKeepsLastGood proves the rejection takes
// the documented reload path: the restriction in force stays in force.
func TestMetadataKeys_WrongTypeOnReloadKeepsLastGood(t *testing.T) {
	path := writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\nallowed_agents = [\"scout\"]\n")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.WriteFile(path, []byte("[svc]\ntoken = \"sk-x-secret\"\nallowed_agents = 90125\n# changed size\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ForAgent("intruder").Get("svc.token"); ok {
		t.Error("the section became unrestricted on reload")
	}
	if _, ok := s.ForAgent("scout").Get("svc.token"); !ok {
		t.Error("the last good contents were not kept")
	}
}

// TestMetadataKeys_SaveKeepsRestriction proves Save writes a string form back
// as an array and that no restriction opens across a Save. The empty list is
// the hard case: it denies every agent, and Save used to drop it.
func TestMetadataKeys_SaveKeepsRestriction(t *testing.T) {
	for _, form := range []string{`""`, `[]`, `"scout, researcher"`} {
		t.Run(form, func(t *testing.T) {
			path := writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\nallowed_agents = "+form+"\n")
			s, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := s.ForAgent("intruder").Get("svc.token"); ok {
				t.Fatal("unrestricted before Save")
			}
			if err := s.Save(); err != nil {
				t.Fatalf("Save: %v", err)
			}
			again, err := Load(path)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if _, ok := again.ForAgent("intruder").Get("svc.token"); ok {
				data, _ := os.ReadFile(path)
				t.Errorf("Save opened the section:\n%s", data)
			}
			if got := again.Names(); !slices.Equal(got, []string{"svc.token"}) {
				t.Errorf("Names after Save = %v", got)
			}
		})
	}
}

// TestMetadataKeys_AgentEmptyOverrideSurvivesSave proves an agent table's empty
// host list, which replaces the global list, is not dropped by Save: dropping
// it would widen the agent's hosts to the global ones.
func TestMetadataKeys_AgentEmptyOverrideSurvivesSave(t *testing.T) {
	path := writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\nallowed_hosts = [\"global.example.com\"]\n\n[agents.scout.svc]\nallowed_hosts = []\n")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := again.ForAgent("scout").AllowedHosts("svc.token"); len(got) != 0 {
		t.Errorf("the empty override was lost: AllowedHosts = %v", got)
	}
}

// TestSet_RefusesMetadataKey proves the `/secrets set` path cannot write a
// metadata key as a secret, which is one way the string form reaches the file.
func TestSet_RefusesMetadataKey(t *testing.T) {
	s, err := Load(writeSecrets(t, "[svc]\ntoken = \"sk-x-secret\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, key := range []string{"allowed_hosts", "allowed_agents", "denied_agents", "allowed_in_body"} {
		name := "svc." + key
		err := s.Set(name, "api.example.com")
		if err == nil {
			t.Errorf("Set(%q) accepted a metadata key", name)
		} else if !strings.Contains(err.Error(), key) {
			t.Errorf("Set(%q) error does not name the key: %v", name, err)
		}
		if _, ok := s.Get(name); ok {
			t.Errorf("Set(%q) stored a value", name)
		}
	}
	if err := s.Set("svc.allowed_hosts_note", "ok"); err != nil {
		t.Errorf("a key that only starts with a metadata name was refused: %v", err)
	}
}
