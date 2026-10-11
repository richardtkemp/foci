package voice

// STTCredential describes how a client calls an STT provider directly (#2315):
// everything the app needs to transcribe without routing audio through foci.
// The values are the provider's own — the same ones the server's Transcribe
// call uses — so a direct-calling client and the server can never disagree on
// endpoint, model or key.
type STTCredential struct {
	Endpoint   string // transcription URL, exactly what the server itself POSTs
	Model      string // model name (may be empty when the endpoint has one default)
	APIKey     string // bearer token; the server's own resolved key
	Format     string // request-shape name the client must build ("openai-multipart")
	FileField  string // multipart field carrying the audio file
	ModelField string // multipart field carrying the model name
}

// STTCredentialer is implemented by STT providers a client can call directly,
// and by wrappers that delegate to the provider they wrap. The capability is
// declared by the provider, never inferred from concrete types — the same
// doctrine as a delegator Spec — so wrappers stay opaque to their callers and
// a future provider with a different direct-call shape declares it itself.
type STTCredentialer interface {
	STTCredential() STTCredential
}

// The one source for the OpenAI-multipart request shape, shared by
// OpenAISTT.Transcribe (the wire) and OpenAISTT.STTCredential (the
// declaration), so the credential cannot lie about what Transcribe sends.
const (
	sttFormatOpenAIMultipart = "openai-multipart"
	sttFileField             = "file"
	sttModelField            = "model"
)

// CredentialOf returns the direct-call credential for s. ok is false when s is
// nil, exposes no credential, or has no endpoint to call — never issue a
// credential the client cannot use.
func CredentialOf(s STT) (STTCredential, bool) {
	c, ok := s.(STTCredentialer)
	if !ok {
		return STTCredential{}, false
	}
	cred := c.STTCredential()
	if cred.Endpoint == "" {
		return STTCredential{}, false
	}
	return cred, true
}
