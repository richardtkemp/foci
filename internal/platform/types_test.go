package platform

import (
	"context"
	"testing"

	"foci/internal/config"
	"foci/internal/warnings"
)

// TestSenderInterface verifies that mockSender implements the Sender interface.
// This is a compile-time check that catches interface drift.
func TestSenderInterface(t *testing.T) {
	var _ Sender = (*mockSender)(nil)
}

// TestConnectionInterface verifies that mockConnection implements the Connection interface.
// This is a compile-time check that catches interface drift.
func TestConnectionInterface(t *testing.T) {
	var _ Connection = (*mockConnection)(nil)
}

// TestMessageHandlerInterface verifies that mockHandler implements the
// MessageHandler interface AND invokes every method on the mock via
// interface dispatch. The method calls below ensure deadcode's RTA can
// trace each method as reachable from a test entry point — without them,
// the unused stubs get flagged as dead code even with -test.
func TestMessageHandlerInterface(t *testing.T) {
	var h MessageHandler = &mockHandler{}
	_ = h.HandleMessage(context.Background(), "", nil, nil)
	_ = h.TransformMessage("")
	_ = h.Warnings()
}

// TestConnectionManagerInterface verifies that noopConnMgr implements ConnectionManager.
// This is a compile-time check that catches interface drift.
func TestConnectionManagerInterface(t *testing.T) {
	var _ ConnectionManager = (*noopConnMgr)(nil)
	var _ ConnectionManager = (*aggregatingConnMgr)(nil)
}

type mockSender struct{}

func (m *mockSender) SessionKey() string                                               { return "" }
func (m *mockSender) SendText(text string) error                                       { return nil }
func (m *mockSender) SendDocument(filePath, caption string) error                      { return nil }
func (m *mockSender) SendVoice(filePath string) error                                  { return nil }
func (m *mockSender) SendVideo(filePath, caption string) error                         { return nil }
func (m *mockSender) SendPhoto(filePath, caption string) error                         { return nil }
func (m *mockSender) SendAudio(filePath, caption string) error                         { return nil }
func (m *mockSender) SendAnimation(filePath, caption string) error                     { return nil }
func (m *mockSender) SendVoiceData(audioData []byte) error                             { return nil }
func (m *mockSender) SendTextToChat(chatID int64, text string) error                   { return nil }
func (m *mockSender) SendDocumentToChat(chatID int64, filePath, caption string) error  { return nil }
func (m *mockSender) SendVoiceToChat(chatID int64, filePath string) error              { return nil }
func (m *mockSender) SendVideoToChat(chatID int64, filePath, caption string) error     { return nil }
func (m *mockSender) SendPhotoToChat(chatID int64, filePath, caption string) error     { return nil }
func (m *mockSender) SendAudioToChat(chatID int64, filePath, caption string) error     { return nil }
func (m *mockSender) SendAnimationToChat(chatID int64, filePath, caption string) error { return nil }
func (m *mockSender) SendVoiceDataToChat(chatID int64, audioData []byte) error         { return nil }

type mockConnection struct {
	*mockSender
}

func (m *mockConnection) PlatformName() string                      { return "mock" }
func (m *mockConnection) SessionKeyForChat(chatID int64) string     { return "" }
func (m *mockConnection) DefaultSessionKey() string                 { return "" }
func (m *mockConnection) SetSessionKey(key string)                  {}
func (m *mockConnection) SetSessionKeyDirect(key string)            {}
func (m *mockConnection) SetChatID(chatID int64)                    {}
func (m *mockConnection) ChatID() int64                             { return 0 }
func (m *mockConnection) Username() string                          { return "" }
func (m *mockConnection) UpdateChatSessionKey(int64, string)        {}
func (m *mockConnection) SendInjectedMessage(sk, text string) error { return nil }
func (m *mockConnection) SendToSession(sk, text string) error       { return nil }
func (m *mockConnection) SendNotification(text string)              {}
func (m *mockConnection) SendNotificationDirect(text string) string { return "" }
func (m *mockConnection) SetTyping(bool)                            {}

type mockHandler struct{}

func (m *mockHandler) HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []Attachment) error {
	return nil
}
func (m *mockHandler) TransformMessage(text string) string { return text }
func (m *mockHandler) Warnings() *warnings.Queue           { return nil }

// --- mockProvider implements MessagingProvider for testing ---

type mockProvider struct {
	name string
}

func (p *mockProvider) Name() string                                            { return p.name }
func (p *mockProvider) IsConfigured(*config.Config) (bool, string)              { return false, "mock" }
func (p *mockProvider) Init(ProviderDeps) error                                 { return nil }
func (p *mockProvider) ConnectionManager() ConnectionManager                    { return &noopConnMgr{} }
func (p *mockProvider) SetupAgentConnection(AgentConnectionParams) *SetupResult { return nil }
func (p *mockProvider) SetupSharedFacet(SharedFacetParams)                      {}
func (p *mockProvider) RestoreFacetSessions(RestoreParams)                      {}
func (p *mockProvider) SetLifecycleCallback(string, LifecycleEvent, func())     {}
func (p *mockProvider) ToolDetailStore() ToolDetailStore                        { return nil }
func (p *mockProvider) AgentPreFlight(string) []string                          { return nil }
func (p *mockProvider) DefaultPlatformConfig() config.PlatformConfig            { return config.PlatformConfig{} }
func (p *mockProvider) ValidateConfig(config.PlatformConfig) []string           { return nil }
func (p *mockProvider) Close() error                                            { return nil }

// mockWizardProvider implements both MessagingProvider and SetupWizard.
type mockWizardProvider struct {
	mockProvider
	flags []SetupFlag
}

func (p *mockWizardProvider) SetupFlags() []SetupFlag { return p.flags }
func (p *mockWizardProvider) RunSetup(ui SetupUI, flags map[string]string, nonInteractive bool) (*WizardResult, error) {
	return &WizardResult{ConfigTOML: "[mock]\n", Secrets: map[string]string{"mock.key": "val"}}, nil
}

// Verifies SetupProviders returns only providers implementing SetupWizard,
// and returns them in sorted order by provider name.
func TestSetupProviders(t *testing.T) {
	// Save and restore registry state
	registryMu.Lock()
	saved := providers
	providers = make(map[string]MessagingProvider)
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		providers = saved
		registryMu.Unlock()
	}()

	// Register a mix: one wizard, one plain provider
	RegisterMessagingProvider("zebra", &mockWizardProvider{
		mockProvider: mockProvider{name: "zebra"},
		flags:        []SetupFlag{{Name: "z-flag", Usage: "zebra flag"}},
	})
	RegisterMessagingProvider("alpha-plain", &mockProvider{name: "alpha-plain"})
	RegisterMessagingProvider("beta", &mockWizardProvider{
		mockProvider: mockProvider{name: "beta"},
		flags:        []SetupFlag{{Name: "b-flag", Usage: "beta flag"}},
	})

	wizards := SetupProviders()
	if len(wizards) != 2 {
		t.Fatalf("got %d wizards, want 2", len(wizards))
	}
	// Should be sorted: beta before zebra
	if wizards[0].Name != "beta" {
		t.Errorf("first wizard name = %q, want beta", wizards[0].Name)
	}
	if wizards[0].Wizard.SetupFlags()[0].Name != "b-flag" {
		t.Errorf("first wizard flag = %q, want b-flag", wizards[0].Wizard.SetupFlags()[0].Name)
	}
	if wizards[1].Name != "zebra" {
		t.Errorf("second wizard name = %q, want zebra", wizards[1].Name)
	}
	if wizards[1].Wizard.SetupFlags()[0].Name != "z-flag" {
		t.Errorf("second wizard flag = %q, want z-flag", wizards[1].Wizard.SetupFlags()[0].Name)
	}
}

// Verifies Messaging facade methods are nil-safe (no panic on nil receiver).
func TestMessagingNilSafe(t *testing.T) {
	var m *Messaging

	// All methods should be safe on nil
	if cm := m.ConnectionManager(); cm == nil {
		t.Error("ConnectionManager on nil should return noopConnMgr, not nil")
	}
	if names := m.ActivePlatformNames(); names != nil {
		t.Errorf("ActivePlatformNames on nil = %v, want nil", names)
	}
	if results := m.SetupAgentConnection(AgentConnectionParams{}); results != nil {
		t.Errorf("SetupAgentConnection on nil = %v, want nil", results)
	}
	m.SetupSharedFacet(SharedFacetParams{})
	m.RestoreFacetSessions(RestoreParams{})
	m.SetLifecycleCallback("x", OnUserMessage, func() {})
	m.NotifyAgent("x", "text")
	m.notifyAgentDoc("x", "/tmp/doc")
	if warns := m.AgentPreFlight("x"); warns != nil {
		t.Errorf("AgentPreFlight on nil = %v, want nil", warns)
	}
	if s := m.ToolDetailStore(); s != nil {
		t.Error("ToolDetailStore on nil should return nil")
	}
	m.StartAll(context.Background())
	m.wait()
	if err := m.Close(); err != nil {
		t.Errorf("Close on nil = %v, want nil", err)
	}
}

// Verifies noopConnMgr returns expected zero values.
func TestNoopConnMgr(t *testing.T) {
	n := &noopConnMgr{}
	if n.Primary("x") != nil {
		t.Error("Primary should return nil")
	}
	if n.AllForAgent("x") != nil {
		t.Error("AllForAgent should return nil")
	}
	if n.ForSession("x") != nil {
		t.Error("ForSession should return nil")
	}
	if n.ForSessionOrPrimary("x", "y") != nil {
		t.Error("ForSessionOrPrimary should return nil")
	}
	if _, ok := n.AcquireFacet("x"); ok {
		t.Error("AcquireFacet should return false")
	}
	if n.HasFacet("x") {
		t.Error("HasFacet should return false")
	}
	// These should not panic
	n.StartAll(context.Background())
	n.Wait()
}

// Verifies aggregatingConnMgr delegates to child managers correctly.
func TestAggregatingConnMgr(t *testing.T) {
	// With no providers, everything returns nil/false
	mgr := newAggregatingConnMgr(nil, nil, nil)
	if mgr.Primary("x") != nil {
		t.Error("Primary with no managers should return nil")
	}
	if mgr.ForSession("x") != nil {
		t.Error("ForSession with no managers should return nil")
	}
	if mgr.ForSessionOrPrimary("x", "y") != nil {
		t.Error("ForSessionOrPrimary with no managers should return nil")
	}
	if _, ok := mgr.AcquireFacet("x"); ok {
		t.Error("AcquireFacet with no managers should return false")
	}
	if mgr.HasFacet("x") {
		t.Error("HasFacet with no managers should return false")
	}
	if conns := mgr.AllForAgent("x"); len(conns) != 0 {
		t.Errorf("AllForAgent with no managers = %d conns, want 0", len(conns))
	}
	// These should not panic
	mgr.StartAll(context.Background())
	mgr.Wait()
}

// TestIsSilent covers the "entirely silent" behaviour: empty and
// whitespace-only text, the [[NO_RESPONSE]] sentinel (possibly padded or
// stacked), and CC's synthetic "No response requested." message are silent.
// Text with real content — including a real reply that *ends with* a sentinel
// — is NOT silent (IsSilent == false); such text is delivered after
// StripSilencingSuffix removes the trailing marker.
func TestIsSilent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"whitespace_only", "   \n\t ", true},
		{"no_response_exact", "[[NO_RESPONSE]]", true},
		{"no_response_padded", "  [[NO_RESPONSE]] \n", true},
		{"no_response_requested", "No response requested.", true},
		{"stacked_sentinels", "[[NO_RESPONSE]][[NO_RESPONSE]]", true},
		{"prefix_only", "[[NO_RESP", false},
		{"with_trailing_text", "[[NO_RESPONSE]] OK", false},
		{"real_text_trailing_sentinel", "real reply [[NO_RESPONSE]]", false},
		{"backtick_wrapped", "`[[NO_RESPONSE]]`", true},
		{"bold_wrapped", "**[[NO_RESPONSE]]**", true},
		{"italic_wrapped", "_[[NO_RESPONSE]]_", true},
		{"backtick_wrapped_padded", "  `[[NO_RESPONSE]]`  \n", true},
		{"backtick_real_text_not_silent", "real reply `[[NO_RESPONSE]]`", false},
		{"bold_text_not_silent", "**bold**", false},
		{"normal_text", "Hello, world", false},
		{"diverged_prefix", "[[NO_RESPITE]]", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSilent(tc.in); got != tc.want {
				t.Errorf("IsSilent(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripSilencingSuffix covers trailing-sentinel removal: a real reply that
// ends with one or more silencing sentinels keeps its content with the
// marker(s) stripped; text that is entirely sentinel(s) collapses to "";
// sentinels that are not at the end (or absent) leave the text untouched.
func TestStripSilencingSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace_only", "  \n\t ", ""},
		{"pure_sentinel", "[[NO_RESPONSE]]", ""},
		{"pure_sentinel_padded", "  [[NO_RESPONSE]] \n", ""},
		{"pure_no_response_requested", "No response requested.", ""},
		{"trailing_space_sep", "real reply [[NO_RESPONSE]]", "real reply"},
		{"trailing_newline_sep", "real reply\n[[NO_RESPONSE]]", "real reply"},
		{"trailing_then_whitespace", "real reply [[NO_RESPONSE]]  \n", "real reply"},
		{"stacked_trailing", "done [[NO_RESPONSE]][[NO_RESPONSE]]", "done"},
		{"stacked_trailing_padded", "done [[NO_RESPONSE]] [[NO_RESPONSE]]", "done"},
		{"mixed_trailing_sentinels", "done No response requested.[[NO_RESPONSE]]", "done"},
		{"no_sentinel", "hello world", "hello world"},
		{"sentinel_mid_text_untouched", "a [[NO_RESPONSE]] b", "a [[NO_RESPONSE]] b"},
		{"sentinel_leading_text_following", "[[NO_RESPONSE]] then more", "[[NO_RESPONSE]] then more"},
		{"clean_text_idempotent", "real reply", "real reply"},
		// Markdown-wrapped sentinels: the observed leak was an agent emitting
		// `[[NO_RESPONSE]]` (backticks) which the bare-literal matcher missed.
		{"backtick_wrapped_pure", "`[[NO_RESPONSE]]`", ""},
		{"bold_wrapped_pure", "**[[NO_RESPONSE]]**", ""},
		{"italic_wrapped_pure", "_[[NO_RESPONSE]]_", ""},
		{"backtick_wrapped_padded", "  `[[NO_RESPONSE]]`  \n", ""},
		{"backtick_wrapped_trailing", "real reply `[[NO_RESPONSE]]`", "real reply"},
		{"bold_wrapped_trailing", "real reply **[[NO_RESPONSE]]**", "real reply"},
		// Negative cases: decoration is peeled ONLY when it wraps a matched
		// sentinel — plain decorated text must survive verbatim.
		{"trailing_asterisk_preserved", "see footnote *", "see footnote *"},
		{"bold_text_preserved", "`**bold**`", "`**bold**`"},
		{"sentinel_in_backticks_mid_text", "use `[[NO_RESPONSE]]` in code", "use `[[NO_RESPONSE]]` in code"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StripSilencingSuffix(tc.in); got != tc.want {
				t.Errorf("StripSilencingSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripSpuriousPrefix covers leading-junk removal: a standalone leading
// spurious token (the "court" Opus-4.8 decoding artifact) is stripped from the
// start of the text; a bare token collapses to ""; the token embedded in a
// larger word or appearing anywhere but the start is left untouched.
func TestStripSpuriousPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"bare_token", "court", ""},
		{"bare_token_padded", "  court \n", ""},
		{"token_then_newlines_then_text", "court\n\nreal reply", "real reply"},
		{"token_then_space_then_text", "court actual content", "actual content"},
		{"leading_ws_then_token_then_text", "\n court\nrest", "rest"},
		// Negative cases: must NOT strip.
		{"embedded_in_word", "courthouse rules", "courthouse rules"},
		{"courtship_untouched", "courtship is old", "courtship is old"},
		{"token_not_at_start", "see you in court", "see you in court"},
		{"normal_text", "hello world", "hello world"},
		{"capitalized_word_untouched", "Court adjourned", "Court adjourned"},
		{"clean_text_idempotent", "real reply", "real reply"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StripSpuriousPrefix(tc.in); got != tc.want {
				t.Errorf("StripSpuriousPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsSilencingPrefix covers the streaming gate: returns true while the
// accumulated buffer could still resolve to a silencing sentinel, false
// once divergence is established. The streaming transport uses this to
// hold delivery during the prefix-ambiguous window at the start of a
// stream, then release once it's clear the turn isn't being silenced.
func TestIsSilencingPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty_could_be_anything", "", true},
		{"whitespace_only", "  \n", true},
		{"single_bracket", "[", true},
		{"two_brackets", "[[", true},
		{"partial_no_response", "[[NO_RESP", true},
		{"full_sentinel_held", "[[NO_RESPONSE]]", true},
		{"full_sentinel_padded_held", "  [[NO_RESPONSE]] \n", true},
		{"diverged_letter", "[[NO_RESPI", false},
		{"sentinel_then_text", "[[NO_RESPONSE]] continuing", false},
		{"normal_text_first_char", "H", false},
		{"normal_text", "Hello", false},
		{"second_sentinel_partial", "No", true},
		{"second_sentinel_full", "No response requested.", true},
		{"second_sentinel_diverged", "No response requested. now what", false},
		{"prefix_with_leading_whitespace", "\n\t  [", true},
		{"leading_backtick_then_bracket", "`[", true},
		{"backtick_partial_sentinel", "`[[NO_RESP", true},
		{"backtick_diverged", "`[[NO_RESPI", false},
		{"bold_text_diverged", "**Bold", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSilencingPrefix(tc.in); got != tc.want {
				t.Errorf("IsSilencingPrefix(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
