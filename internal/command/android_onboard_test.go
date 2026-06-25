package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAndroidWizard(store SecretsStore, configPath string) *androidWizard {
	return &androidWizard{
		store:      store,
		configPath: configPath,
		genKey:     func() (string, error) { return "alpha-bravo-charlie-delta-echo", nil },
	}
}

func TestAndroidWizard_AutoGenShowKey(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/home/foci/config/foci.toml")
	w.apiKey = "maple-thunder-basket-olive-crane"
	w.step = androidStepKeyDelivery

	resp, done := w.Handle("show")
	if done {
		t.Fatal("wizard should not be done after key delivery")
	}
	if !strings.Contains(resp, w.apiKey) {
		t.Errorf("show response should reveal the key; got: %q", resp)
	}
	if w.step != androidStepHost {
		t.Fatalf("expected step Host, got %d", w.step)
	}

	resp, done = w.Handle("https://app.example.com/")
	if !done {
		t.Fatal("wizard should be done after host")
	}
	if !strings.Contains(resp, "app.example.com") || strings.Contains(resp, "https://") {
		t.Errorf("host should be normalized (no scheme/slash); got: %q", resp)
	}
	if !strings.Contains(resp, "foci://pair?host=app.example.com&key=maple-thunder-basket-olive-crane") {
		t.Errorf("summary should contain the full pairing string; got: %q", resp)
	}
}

func TestAndroidWizard_AutoGenReadKey(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/home/foci/config/foci.toml")
	w.apiKey = "maple-thunder-basket-olive-crane"
	w.step = androidStepKeyDelivery

	resp, _ := w.Handle("read")
	if strings.Contains(resp, w.apiKey) {
		t.Errorf("read response must NOT echo the key; got: %q", resp)
	}
	if !strings.Contains(resp, "/home/foci/config/secrets.toml") {
		t.Errorf("read response should point at secrets.toml; got: %q", resp)
	}

	resp, done := w.Handle("192.168.1.50:18792")
	if !done {
		t.Fatal("wizard should be done after host")
	}
	if strings.Contains(resp, w.apiKey) {
		t.Errorf("summary must not contain the key when 'read' chosen; got: %q", resp)
	}
	if !strings.Contains(resp, "foci://pair?host=192.168.1.50%3A18792") {
		t.Errorf("summary should contain a key-less pairing string with escaped host; got: %q", resp)
	}
}

func TestAndroidWizard_KeyDeliveryRejectsGarbage(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.step = androidStepKeyDelivery
	resp, done := w.Handle("maybe")
	if done {
		t.Fatal("garbage input should re-prompt, not finish")
	}
	if !strings.Contains(strings.ToLower(resp), "show") {
		t.Errorf("re-prompt should mention the valid choices; got: %q", resp)
	}
}

func TestAndroidWizard_EmptyHostReprompts(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.step = androidStepHost
	resp, done := w.Handle("   ")
	if done {
		t.Fatal("empty host should re-prompt, not finish")
	}
	if !strings.Contains(resp, "empty") {
		t.Errorf("expected an empty-host re-prompt; got: %q", resp)
	}
}

func TestAndroidWizard_ConfirmEnableYes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(cfgPath, []byte("[[platforms]]\nid = \"telegram\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &mockSecretsStore{data: map[string]string{}}
	w := newTestAndroidWizard(store, cfgPath)
	w.step = androidStepConfirmEnable

	resp, done := w.Handle("yes")
	if done {
		t.Fatal("should advance to key delivery, not finish")
	}
	if w.step != androidStepKeyDelivery {
		t.Fatalf("expected step KeyDelivery, got %d", w.step)
	}
	if !w.justEnabled {
		t.Error("justEnabled should be set after enabling")
	}

	// Config file should now contain the app platform entry.
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "id = \"app\"") {
		t.Errorf("foci.toml should have the app platform appended; got: %q", string(got))
	}
	// Key should have been generated + saved.
	if v, ok := store.Get("app.api_key"); !ok || v != "alpha-bravo-charlie-delta-echo" {
		t.Errorf("expected generated key stored; got %q ok=%v", v, ok)
	}
	if !store.saved {
		t.Error("store.Save() should have been called")
	}
	if !strings.Contains(resp, "enabled") {
		t.Errorf("response should confirm enablement; got: %q", resp)
	}

	// After enabling, the host step must NOT finish — it advances to the
	// restart-confirm step (the /app endpoints aren't live until a restart).
	_, _ = w.Handle("show")
	afterHost, done := w.Handle("app.example.com")
	if done {
		t.Fatal("should advance to restart-confirm after host when justEnabled, not finish")
	}
	if w.step != androidStepConfirmRestart {
		t.Fatalf("expected step ConfirmRestart, got %d", w.step)
	}
	if !strings.Contains(strings.ToLower(afterHost), "restart") {
		t.Errorf("host step should ask to restart after enabling; got: %q", afterHost)
	}
}

func TestAndroidWizard_ConfirmRestartYes(t *testing.T) {
	// Restore the package restart hook after the test.
	orig := restartFunc
	defer func() { restartFunc = orig }()
	called := false
	restartFunc = func() (string, error) {
		called = true
		return "Restarting via systemctl...", nil
	}

	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.justEnabled = true
	w.step = androidStepConfirmRestart

	resp, done := w.Handle("yes")
	if !done {
		t.Fatal("'yes' should finish the wizard")
	}
	if !called {
		t.Error("restartFunc should have been invoked on 'yes'")
	}
	if !strings.Contains(resp, "Restarting") {
		t.Errorf("response should carry the restart message; got: %q", resp)
	}
}

func TestAndroidWizard_ConfirmRestartNo(t *testing.T) {
	orig := restartFunc
	defer func() { restartFunc = orig }()
	called := false
	restartFunc = func() (string, error) { called = true; return "", nil }

	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.justEnabled = true
	w.step = androidStepConfirmRestart

	resp, done := w.Handle("no")
	if !done {
		t.Fatal("'no' should finish the wizard")
	}
	if called {
		t.Error("restartFunc must NOT be invoked on 'no'")
	}
	if !strings.Contains(strings.ToLower(resp), "/restart") {
		t.Errorf("'no' should point at manual /restart; got: %q", resp)
	}
}

func TestAndroidWizard_ConfirmEnableNo(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.step = androidStepConfirmEnable
	resp, done := w.Handle("no")
	if !done {
		t.Fatal("'no' should end the wizard")
	}
	if !strings.Contains(strings.ToLower(resp), "cancel") {
		t.Errorf("expected a cancellation message; got: %q", resp)
	}
}

func TestNormalizeAndroidHost(t *testing.T) {
	cases := map[string]string{
		"https://app.example.com/": "app.example.com",
		"wss://1.2.3.4:18792":      "1.2.3.4:18792",
		"http://nuc.local/":        "nuc.local",
		"  app.example.com  ":      "app.example.com",
		"ws://10.0.0.5:8080/":      "10.0.0.5:8080",
	}
	for in, want := range cases {
		if got := normalizeAndroidHost(in); got != want {
			t.Errorf("normalizeAndroidHost(%q) = %q, want %q", in, got, want)
		}
	}
}
