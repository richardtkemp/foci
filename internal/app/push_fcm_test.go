package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/platform"
)

// fakeSecretGetter is a minimal in-memory config.SecretGetter for tests — no
// real secrets.Store (and no filesystem/group-permission dependency) needed to
// exercise the FCM credential-resolution precedence.
type fakeSecretGetter map[string]string

func (f fakeSecretGetter) Get(key string) (string, bool) {
	v, ok := f[key]
	return v, ok
}

// syntheticPrivateKeyEscaped is a fake, never-used RSA-shaped PEM body with
// literal two-char `\n` escapes, as it would appear after being copied out of
// a JSON file and pasted into a single-line secrets.toml string — the classic
// footgun normalizeFCMPrivateKey exists to fix. It is NOT a real key.
const syntheticPrivateKeyEscaped = "-----BEGIN PRIVATE KEY-----\\nMIIBSYNTHETICNOTAREALKEYDONOTUSEFORANYTHINGWHATSOEVER0000000000\\n-----END PRIVATE KEY-----\\n"

func TestNormalizeFCMPrivateKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "literal backslash-n escapes become real newlines",
			in:   "-----BEGIN PRIVATE KEY-----\\nABC\\n-----END PRIVATE KEY-----\\n",
			want: "-----BEGIN PRIVATE KEY-----\nABC\n-----END PRIVATE KEY-----\n",
		},
		{
			name: "already-real newlines are left untouched",
			in:   "-----BEGIN PRIVATE KEY-----\nABC\n-----END PRIVATE KEY-----\n",
			want: "-----BEGIN PRIVATE KEY-----\nABC\n-----END PRIVATE KEY-----\n",
		},
		{
			name: "empty string stays empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeFCMPrivateKey(tc.in); got != tc.want {
				t.Errorf("normalizeFCMPrivateKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewFCMPusherFromFields(t *testing.T) {
	ctx := context.Background()
	tokens := newPushTokens()

	t.Run("all required fields present builds a pusher", func(t *testing.T) {
		fields := fcmCredentialFields{
			ProjectID:    "synthetic-project",
			ClientEmail:  "fake@synthetic-project.iam.gserviceaccount.com",
			PrivateKey:   syntheticPrivateKeyEscaped,
			PrivateKeyID: "fake-key-id",
		}
		p := newFCMPusherFromFields(ctx, fields, tokens, time.Second)
		if p == nil {
			t.Fatal("expected a pusher, got nil")
		}
		if p.projectID != "synthetic-project" {
			t.Errorf("projectID = %q, want %q", p.projectID, "synthetic-project")
		}
	})

	t.Run("missing project_id disables push", func(t *testing.T) {
		fields := fcmCredentialFields{
			ClientEmail: "fake@synthetic-project.iam.gserviceaccount.com",
			PrivateKey:  syntheticPrivateKeyEscaped,
		}
		if p := newFCMPusherFromFields(ctx, fields, tokens, time.Second); p != nil {
			t.Errorf("expected nil pusher with no project_id, got %+v", p)
		}
	})

	t.Run("missing client_email disables push", func(t *testing.T) {
		fields := fcmCredentialFields{
			ProjectID:  "synthetic-project",
			PrivateKey: syntheticPrivateKeyEscaped,
		}
		if p := newFCMPusherFromFields(ctx, fields, tokens, time.Second); p != nil {
			t.Errorf("expected nil pusher with no client_email, got %+v", p)
		}
	})

	t.Run("missing private_key disables push", func(t *testing.T) {
		fields := fcmCredentialFields{
			ProjectID:   "synthetic-project",
			ClientEmail: "fake@synthetic-project.iam.gserviceaccount.com",
		}
		if p := newFCMPusherFromFields(ctx, fields, tokens, time.Second); p != nil {
			t.Errorf("expected nil pusher with no private_key, got %+v", p)
		}
	})

	t.Run("private_key newline escapes are normalized before use", func(t *testing.T) {
		fields := fcmCredentialFields{
			ProjectID:   "synthetic-project",
			ClientEmail: "fake@synthetic-project.iam.gserviceaccount.com",
			PrivateKey:  syntheticPrivateKeyEscaped,
		}
		p := newFCMPusherFromFields(ctx, fields, tokens, time.Second)
		if p == nil {
			t.Fatal("expected a pusher, got nil")
		}
		// google.CredentialsFromJSON parses PEM lazily (only on first Token()
		// call), so a successful build here doesn't by itself prove the PEM is
		// well-formed. Directly check the normalization the loader applies.
		if got := normalizeFCMPrivateKey(fields.PrivateKey); got == fields.PrivateKey {
			t.Fatal("test fixture doesn't actually exercise the escaped-newline path")
		}
	})
}

// writeSyntheticServiceAccountFile writes a fake (never-used) service-account
// JSON file with the given project id, for exercising the file-path fallback.
func writeSyntheticServiceAccountFile(t *testing.T, projectID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic-fcm.json")
	sa := map[string]string{
		"type":         "service_account",
		"project_id":   projectID,
		"private_key":  "-----BEGIN PRIVATE KEY-----\nSYNTHETIC-NOT-REAL\n-----END PRIVATE KEY-----\n",
		"client_email": "fake@" + projectID + ".iam.gserviceaccount.com",
	}
	data, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal synthetic service account: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write synthetic service account: %v", err)
	}
	return path
}

func TestNewFCMPusherForApp_Precedence(t *testing.T) {
	ctx := context.Background()
	tokens := newPushTokens()

	t.Run("decomposed secret fields take precedence over config path", func(t *testing.T) {
		pathProject := "from-config-path"
		path := writeSyntheticServiceAccountFile(t, pathProject)
		appCfg := &config.AppSpecific{FCMCredentials: path}
		secrets := fakeSecretGetter{
			"app.fcm_project_id":   "from-secret-fields",
			"app.fcm_client_email": "fake@from-secret-fields.iam.gserviceaccount.com",
			"app.fcm_private_key":  syntheticPrivateKeyEscaped,
		}
		p := newFCMPusherForApp(ctx, appCfg, secrets, tokens, time.Second)
		if p == nil {
			t.Fatal("expected a pusher, got nil")
		}
		if p.projectID != "from-secret-fields" {
			t.Errorf("projectID = %q, want secret-fields to win (%q)", p.projectID, "from-secret-fields")
		}
	})

	t.Run("falls back to config path when secret fields are absent", func(t *testing.T) {
		pathProject := "from-config-path-2"
		path := writeSyntheticServiceAccountFile(t, pathProject)
		appCfg := &config.AppSpecific{FCMCredentials: path}
		p := newFCMPusherForApp(ctx, appCfg, fakeSecretGetter{}, tokens, time.Second)
		if p == nil {
			t.Fatal("expected a pusher, got nil")
		}
		if p.projectID != pathProject {
			t.Errorf("projectID = %q, want %q", p.projectID, pathProject)
		}
	})

	t.Run("falls back to legacy app.fcm_credentials secret path when config path is empty", func(t *testing.T) {
		pathProject := "from-legacy-secret-path"
		path := writeSyntheticServiceAccountFile(t, pathProject)
		secrets := fakeSecretGetter{"app.fcm_credentials": path}
		p := newFCMPusherForApp(ctx, &config.AppSpecific{}, secrets, tokens, time.Second)
		if p == nil {
			t.Fatal("expected a pusher, got nil")
		}
		if p.projectID != pathProject {
			t.Errorf("projectID = %q, want %q", p.projectID, pathProject)
		}
	})

	t.Run("nothing configured disables push", func(t *testing.T) {
		p := newFCMPusherForApp(ctx, &config.AppSpecific{}, fakeSecretGetter{}, tokens, time.Second)
		if p != nil {
			t.Errorf("expected nil pusher, got %+v", p)
		}
	})

	t.Run("nil appCfg and nil secrets disables push gracefully", func(t *testing.T) {
		p := newFCMPusherForApp(ctx, nil, nil, tokens, time.Second)
		if p != nil {
			t.Errorf("expected nil pusher, got %+v", p)
		}
	})
}

// TestNewHub_NilSecretStoreWithLiveCtx guards against a real Go footgun: a nil
// *secrets.Store passed straight into the config.SecretGetter interface
// parameter becomes a non-nil interface wrapping a nil pointer ("typed nil"),
// which would make a naive `secrets != nil` check inside the FCM resolver pass
// and then panic on the first .Get call. newHub must convert deps.SecretStore
// to the interface only when it's genuinely non-nil (see the comment at the
// call site in hub.go). A live (non-nil) Ctx is required to even reach the FCM
// setup code — Ctx nil skips it entirely.
func TestNewHub_NilSecretStoreWithLiveCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newHub panicked with a live Ctx and nil SecretStore: %v", r)
		}
	}()

	h := newHub(platform.ProviderDeps{Ctx: ctx}) // SecretStore intentionally left nil
	if h == nil {
		t.Fatal("expected a Hub, got nil")
	}
	if h.pusher != nil {
		t.Error("expected no pusher with no credentials available")
	}
}
