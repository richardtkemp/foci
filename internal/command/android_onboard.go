package command

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"foci/internal/secrets"
)

// AndroidDeps holds dependencies for the /android onboarding wizard. Mirrors the
// other wizard deps (SecretsDeps, AgentNewDeps): the Registry reference is needed
// so the command can hand control to the stepped wizard via SetWizard.
type AndroidDeps struct {
	Registry *Registry
}

// androidWizard step indices. The flow adapts to current state:
//   - app provider disabled        → ConfirmEnable → KeyDelivery → Host
//   - enabled, auto-generated key  → KeyDelivery → Host
//   - enabled, user-set key        → Host (the user already knows their key)
const (
	androidStepConfirmEnable = iota
	androidStepKeyDelivery
	androidStepHost
)

const androidKeyDeliveryPrompt = "Your master API key is auto-generated. Show it here in chat, or read it yourself from the secrets file?\nReply `show` or `read`:"

const androidHostPrompt = "What host will the device connect to? e.g. `app.richardkemp.uk`, or `192.168.1.50:18792` on the LAN.\nEnter the host:"

// AndroidCommand returns the /android command: an onboarding wizard for the
// native Android (FAP) client. It enables the app provider if needed, surfaces
// the master app.api_key (only when auto-generated, and only how the user
// chooses), collects the host, and emits a pairing string for the device.
func AndroidCommand() *Command {
	return &Command{
		Name:        "android",
		Description: "Onboard the native Android app (pairing wizard)",
		Category:    "session",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			reg := androidRegistry(cc)
			if reg == nil || cc.SecretsStore == nil {
				return Response{Text: "Android onboarding wizard is not available."}, nil
			}

			w := newAndroidWizard(cc)

			// Not enabled yet → offer to enable the app provider first.
			if cc.Config == nil || cc.Config.Platform("app") == nil {
				w.step = androidStepConfirmEnable
				reg.SetWizard(w)
				return Response{Text: "📱 Android onboarding\n\nThe app provider isn't enabled yet. Enabling it appends `[[platforms]] id = \"app\"` to foci.toml and generates a master API key.\n\nEnable it now? (`yes`/`no`)"}, nil
			}

			// Enabled — read the current master key.
			key, _ := cc.SecretsStore.Get("app.api_key")
			key = strings.TrimSpace(key)
			if key == "" {
				return Response{Text: "📱 The app provider is enabled but no `app.api_key` is set. Restart foci (`/restart`) to auto-generate it, then run /android again."}, nil
			}
			w.apiKey = key

			reg.SetWizard(w)
			if secrets.IsGeneratedPassphrase(key) {
				w.step = androidStepKeyDelivery
				return Response{Text: "📱 Android onboarding\n\n" + androidKeyDeliveryPrompt}, nil
			}
			// User-set key: they already know it; skip straight to the host.
			w.step = androidStepHost
			return Response{Text: "📱 Android onboarding\n\nUsing your existing (user-set) app.api_key.\n\n" + androidHostPrompt}, nil
		},
	}
}

func androidRegistry(cc CommandContext) *Registry {
	if cc.AndroidDeps != nil {
		return cc.AndroidDeps.Registry
	}
	return nil
}

// androidWizard implements WizardHandler for interactive Android onboarding.
type androidWizard struct {
	step       int
	store      SecretsStore
	configPath string

	apiKey      string
	showKey     bool // user chose to reveal the key in chat
	justEnabled bool // app provider was enabled during this wizard run
	host        string

	// genKey is overridable for testing; defaults to a 5-word EFF passphrase
	// (same scheme as secrets_init.go's auto-generation).
	genKey func() (string, error)
}

func newAndroidWizard(cc CommandContext) *androidWizard {
	return &androidWizard{
		store:      cc.SecretsStore,
		configPath: cc.ConfigPath,
		genKey:     func() (string, error) { return secrets.GeneratePassphrase(5) },
	}
}

// Handle processes a wizard step and returns the response.
func (w *androidWizard) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)
	switch w.step {
	case androidStepConfirmEnable:
		return w.handleConfirmEnable(text)
	case androidStepKeyDelivery:
		return w.handleKeyDelivery(text)
	case androidStepHost:
		return w.handleHost(text)
	default:
		return "Unexpected state.", true
	}
}

func (w *androidWizard) handleConfirmEnable(text string) (string, bool) {
	switch strings.ToLower(text) {
	case "yes", "y":
		// Append a minimal app platform entry; DefaultPlatformConfig fills the rest.
		if err := appendToFile(w.configPath, "\n[[platforms]]\nid = \"app\"\n", 0); err != nil {
			return fmt.Sprintf("Failed to enable app provider: %s", err), true
		}
		// Generate the master key NOW (so it's retrievable without waiting for a
		// restart), unless one somehow already exists.
		key, ok := w.store.Get("app.api_key")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			gen, err := w.genKey()
			if err != nil {
				return fmt.Sprintf("Enabled, but failed to generate key: %s", err), true
			}
			w.store.Set("app.api_key", gen)
			if err := w.store.Save(); err != nil {
				return fmt.Sprintf("Enabled, but failed to save key: %s", err), true
			}
			key = gen
		}
		w.apiKey = key
		w.justEnabled = true
		w.step = androidStepKeyDelivery
		return "✅ App provider enabled in foci.toml and master key ready.\n\n" + androidKeyDeliveryPrompt, false
	case "no", "n":
		return "Cancelled — nothing changed.", true
	default:
		return "Enable the app provider? Reply `yes` or `no`:", false
	}
}

func (w *androidWizard) handleKeyDelivery(text string) (string, bool) {
	switch strings.ToLower(text) {
	case "show", "1":
		w.showKey = true
		w.step = androidStepHost
		return fmt.Sprintf("🔑 `app.api_key`:\n`%s`\n\n%s", w.apiKey, androidHostPrompt), false
	case "read", "2":
		w.showKey = false
		w.step = androidStepHost
		return fmt.Sprintf("Read it yourself from `%s` → `[app]` section, key `api_key`.\n\n%s", w.secretsPath(), androidHostPrompt), false
	default:
		return "Reply `show` (reveal the key here) or `read` (point me to the secrets file):", false
	}
}

func (w *androidWizard) handleHost(text string) (string, bool) {
	host := normalizeAndroidHost(text)
	if host == "" {
		return "Host can't be empty. Enter the server host (e.g. `app.richardkemp.uk` or `192.168.1.50:18792`):", false
	}
	w.host = host
	return w.finalSummary(), true
}

// secretsPath derives the secrets.toml path from the config path (they live in
// the same directory; see cmd/foci-gw/secrets_init.go).
func (w *androidWizard) secretsPath() string {
	if w.configPath == "" {
		return "secrets.toml"
	}
	return filepath.Join(filepath.Dir(w.configPath), "secrets.toml")
}

func (w *androidWizard) finalSummary() string {
	var sb strings.Builder
	sb.WriteString("📱 Android pairing\n\n")
	fmt.Fprintf(&sb, "Host: `%s`\n", w.host)

	if w.showKey {
		fmt.Fprintf(&sb, "Key:  `%s`\n", w.apiKey)
		pair := fmt.Sprintf("foci://pair?host=%s&key=%s", url.QueryEscape(w.host), url.QueryEscape(w.apiKey))
		fmt.Fprintf(&sb, "\nPairing string (scan or paste in the app):\n`%s`\n", pair)
	} else {
		sb.WriteString("Key:  (read from the secrets file — enter it manually in the app)\n")
		pair := fmt.Sprintf("foci://pair?host=%s", url.QueryEscape(w.host))
		fmt.Fprintf(&sb, "\nPairing string (key entered manually in the app):\n`%s`\n", pair)
	}

	sb.WriteString("\nNext steps:\n")
	sb.WriteString("1. Install the debug APK on your device.\n")
	sb.WriteString("2. In the app, enter the host + key (or scan the pairing string).\n")
	sb.WriteString("3. Tap pair — the device swaps the master key for its own revocable token.\n")

	if w.justEnabled {
		sb.WriteString("\n⚠️ You just enabled the app provider — restart foci (`/restart`) to bring up the `/app` endpoints before pairing.")
	}
	return sb.String()
}

// normalizeAndroidHost strips a URL scheme and trailing slash, mirroring the
// Android client's PairingViewModel input cleaning, so a pasted URL works.
func normalizeAndroidHost(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://", "wss://", "ws://"} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimRight(s, "/")
}
