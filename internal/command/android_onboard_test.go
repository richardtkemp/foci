package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
)

// testMintPairKey is a stand-in for the live hub's pairing-key minter: it returns
// a fixed, recognisable key so tests can assert it surfaces in the wizard output.
func testMintPairKey(ttl time.Duration) (string, time.Time, error) {
	return "PAIRKEY-TEST", time.Now().Add(ttl), nil
}

// newTestAndroidWizard builds a wizard with the test pairing-key minter wired in
// (mirrors what newAndroidWizard does from cc.AndroidDeps).
func newTestAndroidWizard(store SecretsStore, configPath string) *androidWizard {
	return &androidWizard{
		store:       store,
		configPath:  configPath,
		mintPairKey: testMintPairKey,
	}
}

//  1. Enabled path: /android jumps straight to the host step, then minting emits
//     the pairing key + foci://pair string.
func TestAndroidWizard_EnabledPath_MintsKey(t *testing.T) {
	reg := NewRegistry()
	cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: "app"}}}
	cc := CommandContext{
		Config:       cfg,
		ConfigPath:   "/home/foci/config/foci.toml",
		SecretsStore: &mockSecretsStore{data: map[string]string{}},
		AndroidDeps:  &AndroidDeps{Registry: reg, MintPairKey: testMintPairKey},
	}

	resp, err := AndroidCommand().Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "host") {
		t.Errorf("enabled path should prompt for the host, got: %q", resp.Text)
	}

	// The wizard is now active; feeding it the host mints + emits the key.
	out, doc, ok := reg.HandleMessage("", "https://app.example.com/")
	if !ok {
		t.Fatal("wizard should be active after /android on the enabled path")
	}
	if !strings.Contains(out, "PAIRKEY-TEST") {
		t.Errorf("summary should contain the minted key; got: %q", out)
	}
	if !strings.Contains(out, "foci://pair?host=app.example.com&key=PAIRKEY-TEST") {
		t.Errorf("summary should contain the full pairing string; got: %q", out)
	}
	if strings.Contains(out, "https://") {
		t.Errorf("host should be normalized (no scheme); got: %q", out)
	}
	// The wizard also hands back a QR image of the pairing string (WizardDocProvider).
	if doc == "" {
		t.Error("wizard should return a QR doc path alongside the summary")
	} else {
		if fi, err := os.Stat(doc); err != nil || fi.Size() == 0 {
			t.Errorf("QR doc should be a non-empty file; path=%q err=%v", doc, err)
		}
		_ = os.Remove(doc)
	}
}

//  2. Not-enabled path: ConfirmEnable → Host → ConfirmRestart (no key emitted yet,
//     because the hub isn't live until a restart) → restart on "yes".
func TestAndroidWizard_NotEnabledPath_EnablesThenRestarts(t *testing.T) {
	orig := restartFunc
	defer func() { restartFunc = orig }()
	called := false
	restartFunc = func() (string, error) { called = true; return "Restarting via systemctl...", nil }

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(cfgPath, []byte("[[platforms]]\nid = \"telegram\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	cc := CommandContext{
		Config:       &config.Config{}, // no app platform → not enabled
		ConfigPath:   cfgPath,
		SecretsStore: &mockSecretsStore{data: map[string]string{}},
		AndroidDeps:  &AndroidDeps{Registry: reg, MintPairKey: testMintPairKey},
	}

	resp, err := AndroidCommand().Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(resp.Text), "enable") {
		t.Errorf("not-enabled path should offer to enable; got: %q", resp.Text)
	}

	// Confirm enabling: appends the app platform and advances to the host step.
	out, _, ok := reg.HandleMessage("", "yes")
	if !ok {
		t.Fatal("wizard should be active to confirm enable")
	}
	if got, _ := os.ReadFile(cfgPath); !strings.Contains(string(got), "id = \"app\"") {
		t.Errorf("foci.toml should have the app platform appended; got: %q", string(got))
	}
	if !strings.Contains(strings.ToLower(out), "enabled") {
		t.Errorf("response should confirm enablement; got: %q", out)
	}

	// Host step after enabling → restart-confirm prompt, NOT a key (hub not live).
	out, _, _ = reg.HandleMessage("", "app.example.com")
	if strings.Contains(out, "PAIRKEY-TEST") {
		t.Errorf("no key should be minted before the restart; got: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "restart") {
		t.Errorf("host step should ask to restart after enabling; got: %q", out)
	}

	// Confirm restart → restartFunc fires and the wizard finishes.
	out, _, _ = reg.HandleMessage("", "yes")
	if !called {
		t.Error("restartFunc should have been invoked on 'yes'")
	}
	if !strings.Contains(out, "Restarting") {
		t.Errorf("response should carry the restart message; got: %q", out)
	}
}

//  3. PairKeyCommand (headless): a working minter returns the key; a nil minter
//     reports the app provider isn't running.
func TestPairKeyCommand(t *testing.T) {
	// With a working minter → the key (and, given a host, a pairing string).
	cc := CommandContext{AndroidDeps: &AndroidDeps{MintPairKey: testMintPairKey}}
	resp, err := PairKeyCommand().Execute(context.Background(), Request{Args: "app.example.com"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "PAIRKEY-TEST") {
		t.Errorf("pairkey response should contain the minted key; got: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "foci://pair?host=app.example.com&key=PAIRKEY-TEST") {
		t.Errorf("pairkey response should contain the pairing string; got: %q", resp.Text)
	}

	// Nil minter → "isn't running" message, no key.
	ccOff := CommandContext{AndroidDeps: &AndroidDeps{MintPairKey: nil}}
	respOff, err := PairKeyCommand().Execute(context.Background(), Request{}, ccOff)
	if err != nil {
		t.Fatalf("Execute (off): %v", err)
	}
	if !strings.Contains(strings.ToLower(respOff.Text), "running") {
		t.Errorf("nil minter should report the app provider isn't running; got: %q", respOff.Text)
	}
}

// handleHost normalizes the host before minting / emitting the pairing string.
func TestAndroidWizard_HostNormalizedInPairingString(t *testing.T) {
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, "/c/foci.toml")
	w.step = androidStepHost

	resp, done := w.Handle("192.168.1.50:18792")
	if !done {
		t.Fatal("wizard should be done after host on the enabled path")
	}
	if !strings.Contains(resp, "foci://pair?host=192.168.1.50%3A18792") {
		t.Errorf("summary should contain a pairing string with the escaped host; got: %q", resp)
	}
	if !strings.Contains(resp, "PAIRKEY-TEST") {
		t.Errorf("summary should contain the minted key; got: %q", resp)
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
	w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, cfgPath)
	w.step = androidStepConfirmEnable

	resp, done := w.Handle("yes")
	if done {
		t.Fatal("should advance to the host step, not finish")
	}
	if w.step != androidStepHost {
		t.Fatalf("expected step Host, got %d", w.step)
	}
	if !w.justEnabled {
		t.Error("justEnabled should be set after enabling")
	}
	if got, _ := os.ReadFile(cfgPath); !strings.Contains(string(got), "id = \"app\"") {
		t.Errorf("foci.toml should have the app platform appended; got: %q", string(got))
	}
	if !strings.Contains(resp, "enabled") {
		t.Errorf("response should confirm enablement; got: %q", resp)
	}

	// After enabling, the host step advances to restart-confirm (no key yet).
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

func TestAndroidWizard_ConfirmRestartYes(t *testing.T) {
	orig := restartFunc
	defer func() { restartFunc = orig }()
	called := false
	restartFunc = func() (string, error) { called = true; return "Restarting via systemctl...", nil }

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
