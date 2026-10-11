package voice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAISTT_CredentialMatchesWire proves the credential cannot lie about
// the wire: every field STTCredential declares (endpoint, key, model, format,
// file and model field names) is cross-checked against the multipart request
// Transcribe actually sends. Diverging a shape constant or a Transcribe
// literal fails this from either side.
func TestOpenAISTT_CredentialMatchesWire(t *testing.T) {
	const (
		apiKey = "stt-wire-key-123"
		model  = "whisper-large-v3"
	)
	var gotPath, gotAuth, gotFileField, gotFilename, wireModelField string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		for field, vals := range r.MultipartForm.Value {
			if len(vals) > 0 && vals[0] == model {
				wireModelField = field
			}
		}
		for field, fhs := range r.MultipartForm.File {
			gotFileField = field
			if len(fhs) > 0 {
				gotFilename = fhs[0].Filename
			}
		}
		_, _ = w.Write([]byte("  transcript text  "))
	}))
	defer srv.Close()

	s := &OpenAISTT{Endpoint: srv.URL + "/v1/audio/transcriptions", APIKey: apiKey, Model: model}
	text, err := s.Transcribe(context.Background(), []byte("audio-bytes"), "voice.ogg")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "transcript text" {
		t.Errorf("transcript = %q, want trimmed %q", text, "transcript text")
	}

	cred, ok := CredentialOf(s)
	if !ok {
		t.Fatal("CredentialOf(*OpenAISTT) = !ok, want the provider's credential")
	}

	// What the wire carried.
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("request path = %q, want the configured endpoint's", gotPath)
	}
	if gotAuth != "Bearer "+apiKey {
		t.Errorf("authorization = %q, want the Bearer of the configured key", gotAuth)
	}
	if gotFilename != "voice.ogg" {
		t.Errorf("uploaded filename = %q, want the caller's", gotFilename)
	}
	if gotFileField != "file" {
		t.Errorf("wire file field = %q, want the OpenAI-compatible literal %q", gotFileField, "file")
	}
	if wireModelField != "model" {
		t.Errorf("wire model field = %q, want %q", wireModelField, "model")
	}

	// What the credential declares, field by field against the wire.
	if cred.Endpoint != s.Endpoint {
		t.Errorf("credential endpoint = %q, want the URL Transcribe POSTs to (%q)", cred.Endpoint, s.Endpoint)
	}
	if cred.APIKey != apiKey {
		t.Errorf("credential key = %q, want the bearer Transcribe sends", cred.APIKey)
	}
	if cred.Model != model {
		t.Errorf("credential model = %q, want %q", cred.Model, model)
	}
	if cred.FileField != gotFileField {
		t.Errorf("file field: credential = %q, wire = %q, want the same name on both", cred.FileField, gotFileField)
	}
	if cred.ModelField != wireModelField {
		t.Errorf("model field: credential = %q, wire = %q, want the same name on both", cred.ModelField, wireModelField)
	}
	if cred.Format != "openai-multipart" {
		t.Errorf("credential format = %q, want %q", cred.Format, "openai-multipart")
	}
}

// TestCredentialOf_NilAndNonExposer covers the false arms that are not the
// empty-endpoint one: a nil provider and a provider that declares no
// direct-call shape both yield ok=false — nothing to issue.
func TestCredentialOf_NilAndNonExposer(t *testing.T) {
	if _, ok := CredentialOf(nil); ok {
		t.Error("CredentialOf(nil) = ok, want false")
	}
	if _, ok := CredentialOf(&sttFake{}); ok {
		t.Error("CredentialOf(non-exposing provider) = ok, want false")
	}
}

// sttFake is an STT that declares no direct-call capability.
type sttFake struct{}

var _ STT = (*sttFake)(nil)

func (f *sttFake) Transcribe(_ context.Context, _ []byte, _ string) (string, error) { return "", nil }

// TestCredentialOf_EmptyEndpoint proves an unusable credential is never
// issued: NewSTT accepts an empty endpoint today, so the empty-endpoint guard
// is what keeps /app/stt from handing a client a URL it cannot call.
func TestCredentialOf_EmptyEndpoint(t *testing.T) {
	if _, ok := CredentialOf(&OpenAISTT{APIKey: "k", Model: "m"}); ok {
		t.Error("CredentialOf(empty endpoint) = ok, want false")
	}
}

// TestReplacingSTT_CredentialDelegates proves the wrapper stays transparent:
// word replacements post-process text, so the credential is the wrapped
// provider's, unchanged.
func TestReplacingSTT_CredentialDelegates(t *testing.T) {
	inner := &OpenAISTT{Endpoint: "https://stt.example/v1/audio/transcriptions", APIKey: "k", Model: "m"}
	wrapped := WrapSTT(inner, map[string]string{"foci": "Foci"})

	cred, ok := CredentialOf(wrapped)
	if !ok {
		t.Fatal("CredentialOf(WrapSTT(...)) = !ok, want the wrapped provider's credential")
	}
	if want := inner.STTCredential(); cred != want {
		t.Errorf("wrapped credential = %+v, want the inner provider's %+v", cred, want)
	}
}
