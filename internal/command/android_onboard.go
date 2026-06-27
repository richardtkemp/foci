package command

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// AndroidDeps holds dependencies for the /android onboarding wizard. Mirrors the
// other wizard deps (SecretsDeps, AgentNewDeps): the Registry reference is needed
// so the command can hand control to the stepped wizard via SetWizard. MintPairKey
// mints a single-use, in-memory pairing key on the live app hub (#862) — nil when
// the app provider isn't running.
type AndroidDeps struct {
	Registry    *Registry
	MintPairKey func(ttl time.Duration) (key string, expiry time.Time, err error)
}

// androidWizard step indices. The flow adapts to current state:
//   - app provider disabled → ConfirmEnable → Host → ConfirmRestart
//     (after the restart brings the hub up, the user re-runs /android to mint a key)
//   - app provider enabled  → Host (then mint a pairing key and emit it)
//
// ConfirmRestart only runs when the wizard enabled the app provider this run
// (justEnabled): the running server loaded its config before the [[platforms]]
// id="app" line was appended, so the /app endpoints — and the in-memory pairing
// key store — aren't live until a restart. A pairing key can only be minted once
// the hub exists, so we defer minting to the post-restart /android re-run.
const (
	androidStepConfirmEnable = iota
	androidStepHost
	androidStepConfirmRestart
)

const androidHostPrompt = "What host will the device connect to? e.g. `app.richardkemp.uk`, or `192.168.1.50:18792` on the LAN.\nEnter the host:"

// pairKeyTTL is how long a minted pairing key stays valid. Short by design: you
// pair right after running the wizard.
const pairKeyTTL = 10 * time.Minute

// AndroidCommand returns the /android command: an onboarding wizard for the
// native Android (FAP) client. It enables the app provider if needed, collects
// the host, mints a single-use pairing key (#862 — no persisted master key), and
// emits a pairing string for the device.
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
				return Response{Text: "📱 Android onboarding\n\nThe app provider isn't enabled yet. Enabling it appends `[[platforms]] id = \"app\"` to foci.toml.\n\nEnable it now? (`yes`/`no`)"}, nil
			}

			// Enabled — go straight to the host; the pairing key is minted once we
			// have it (the hub is live).
			w.step = androidStepHost
			reg.SetWizard(w)
			return Response{Text: "📱 Android onboarding\n\n" + androidHostPrompt}, nil
		},
	}
}

// PairKeyCommand returns the /pairkey command: mint a single-use Android pairing
// key (#862) without the interactive wizard. This is the headless path — invoked
// from the `foci` CLI (`foci pair-key [host]`) when there's no telegram/discord
// chat to run /android in. The key is returned in the response only (CLI stdout /
// the originating chat); it is never persisted or logged. Optional argument: the
// host, to emit a full `foci://pair?...` string.
func PairKeyCommand() *Command {
	return &Command{
		Name:        "pairkey",
		Description: "Mint a single-use Android pairing key (headless onboarding)",
		Category:    "session",
		Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.AndroidDeps == nil || cc.AndroidDeps.MintPairKey == nil {
				return Response{Text: "App provider isn't running — enable [[platforms]] id=\"app\" and restart first."}, nil
			}
			key, exp, err := cc.AndroidDeps.MintPairKey(pairKeyTTL)
			if err != nil {
				return Response{Text: "Couldn't mint a pairing key: " + err.Error()}, nil
			}
			mins := int(time.Until(exp).Round(time.Minute).Minutes())
			var sb strings.Builder
			fmt.Fprintf(&sb, "Pairing key (valid ~%d min, single use):\n%s\n", mins, key)
			if host := normalizeAndroidHost(req.Args); host != "" {
				pair := fmt.Sprintf("foci://pair?host=%s&key=%s", url.QueryEscape(host), url.QueryEscape(key))
				fmt.Fprintf(&sb, "\nHost: %s\nPairing string:\n%s\n", host, pair)
			}
			return Response{Text: sb.String()}, nil
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

	justEnabled bool // app provider was enabled during this wizard run
	host        string

	// mintPairKey mints a single-use pairing key on the live app hub. nil when
	// the app provider isn't running (e.g. just enabled, awaiting restart).
	mintPairKey func(ttl time.Duration) (string, time.Time, error)
}

func newAndroidWizard(cc CommandContext) *androidWizard {
	w := &androidWizard{
		store:      cc.SecretsStore,
		configPath: cc.ConfigPath,
	}
	if cc.AndroidDeps != nil {
		w.mintPairKey = cc.AndroidDeps.MintPairKey
	}
	return w
}

// Handle processes a wizard step and returns the response.
func (w *androidWizard) Handle(text string) (string, bool) {
	text = strings.TrimSpace(text)
	switch w.step {
	case androidStepConfirmEnable:
		return w.handleConfirmEnable(text)
	case androidStepHost:
		return w.handleHost(text)
	case androidStepConfirmRestart:
		return w.handleConfirmRestart(text)
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
		w.justEnabled = true
		w.step = androidStepHost
		return "✅ App provider enabled in foci.toml.\n\n" + androidHostPrompt, false
	case "no", "n":
		return "Cancelled — nothing changed.", true
	default:
		return "Enable the app provider? Reply `yes` or `no`:", false
	}
}

func (w *androidWizard) handleHost(text string) (string, bool) {
	host := normalizeAndroidHost(text)
	if host == "" {
		return "Host can't be empty. Enter the server host (e.g. `app.richardkemp.uk` or `192.168.1.50:18792`):", false
	}
	w.host = host

	// If we enabled the app provider this run, the /app endpoints (and the
	// pairing-key store) aren't live until foci reloads its config. Defer minting
	// to the post-restart re-run; offer to restart now.
	if w.justEnabled {
		w.step = androidStepConfirmRestart
		return w.preRestartSummary() + "\n\n🔄 foci needs to restart to enable this. Okay to restart now? (`yes`/`no`)", false
	}

	// Hub is live — mint a single-use pairing key and emit the pairing string.
	return w.mintAndSummarize(), true
}

func (w *androidWizard) handleConfirmRestart(text string) (string, bool) {
	switch strings.ToLower(text) {
	case "yes", "y":
		msg, err := restartFunc()
		if err != nil {
			return fmt.Sprintf("Restart failed: %s\nRestart manually with `/restart`, then run /android again to mint your pairing key.", err), true
		}
		return "🔄 " + msg + "\n\nOnce it's back up, run /android again to mint your one-time pairing key.", true
	case "no", "n":
		return "Okay — restart later with `/restart`, then run /android again to mint your pairing key.", true
	default:
		return "Restart foci now? Reply `yes` or `no`:", false
	}
}

// mintAndSummarize mints a single-use pairing key and returns the final pairing
// instructions. The key is shown once here (it's ephemeral, single-use, and
// short-lived) and never persisted or logged.
func (w *androidWizard) mintAndSummarize() string {
	if w.mintPairKey == nil {
		return "📱 The app provider isn't running yet, so I can't mint a pairing key. Restart foci (`/restart`), then run /android again."
	}
	key, exp, err := w.mintPairKey(pairKeyTTL)
	if err != nil {
		return fmt.Sprintf("📱 Couldn't mint a pairing key: %s\nMake sure the app provider is running (`/restart`), then run /android again.", err)
	}

	var sb strings.Builder
	sb.WriteString("📱 Android pairing\n\n")
	fmt.Fprintf(&sb, "Host: `%s`\n", w.host)
	fmt.Fprintf(&sb, "Pairing key (valid ~%d min, single use):\n`%s`\n", int(time.Until(exp).Round(time.Minute).Minutes()), key)
	pair := fmt.Sprintf("foci://pair?host=%s&key=%s", url.QueryEscape(w.host), url.QueryEscape(key))
	fmt.Fprintf(&sb, "\nPairing string (scan or paste in the app):\n`%s`\n", pair)

	sb.WriteString("\nNext steps:\n")
	sb.WriteString("1. Install the APK on your device.\n")
	sb.WriteString("2. In the app, enter the host + pairing key (or scan the pairing string).\n")
	sb.WriteString("3. Tap pair — the device swaps the one-time key for its own revocable token.\n")
	sb.WriteString("\nThe key is single-use and expires; if it lapses, run /android again for a fresh one.")
	return sb.String()
}

// preRestartSummary is shown when the wizard just enabled the app provider and
// must restart before a key can be minted.
func (w *androidWizard) preRestartSummary() string {
	var sb strings.Builder
	sb.WriteString("📱 Android pairing\n\n")
	fmt.Fprintf(&sb, "Host: `%s`\n", w.host)
	sb.WriteString("Pairing key: minted after the restart (the hub isn't live yet).")
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
