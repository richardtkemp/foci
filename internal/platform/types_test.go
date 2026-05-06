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
	_ = h.IsProcessing()
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

func (m *mockSender) SessionKey() string                                       { return "" }
func (m *mockSender) SendText(text string) error                            { return nil }
func (m *mockSender) SendDocument(filePath, caption string) error              { return nil }
func (m *mockSender) SendVoice(filePath string) error                          { return nil }
func (m *mockSender) SendVideo(filePath, caption string) error                 { return nil }
func (m *mockSender) SendPhoto(filePath, caption string) error                 { return nil }
func (m *mockSender) SendAudio(filePath, caption string) error                 { return nil }
func (m *mockSender) SendAnimation(filePath, caption string) error             { return nil }
func (m *mockSender) SendVoiceData(audioData []byte) error                     { return nil }
func (m *mockSender) SendTextToChat(chatID int64, text string) error        { return nil }
func (m *mockSender) SendDocumentToChat(chatID int64, filePath, caption string) error  { return nil }
func (m *mockSender) SendVoiceToChat(chatID int64, filePath string) error             { return nil }
func (m *mockSender) SendVideoToChat(chatID int64, filePath, caption string) error    { return nil }
func (m *mockSender) SendPhotoToChat(chatID int64, filePath, caption string) error    { return nil }
func (m *mockSender) SendAudioToChat(chatID int64, filePath, caption string) error    { return nil }
func (m *mockSender) SendAnimationToChat(chatID int64, filePath, caption string) error { return nil }
func (m *mockSender) SendVoiceDataToChat(chatID int64, audioData []byte) error        { return nil }

type mockConnection struct {
	*mockSender
}

func (m *mockConnection) PlatformName() string                 { return "mock" }
func (m *mockConnection) SessionKeyForChat(chatID int64) string { return "" }
func (m *mockConnection) DefaultSessionKey() string             { return "" }
func (m *mockConnection) SetSessionKey(key string)              {}
func (m *mockConnection) SetSessionKeyDirect(key string)        {}
func (m *mockConnection) SetChatID(chatID int64)                {}
func (m *mockConnection) ChatID() int64                         { return 0 }
func (m *mockConnection) Username() string                      { return "" }
func (m *mockConnection) UpdateChatSessionKey(int64, string)     {}
func (m *mockConnection) SendInjectedMessage(sk, text string) error { return nil }
func (m *mockConnection) SendToSession(sk, text string) error       { return nil }
func (m *mockConnection) SendNotification(text string)            {}
func (m *mockConnection) SendNotificationDirect(text string) string { return "" }
func (m *mockConnection) SetTyping(bool)                           {}

type mockHandler struct{}

func (m *mockHandler) HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []Attachment) error {
	return nil
}
func (m *mockHandler) IsProcessing() bool                  { return false }
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
func (p *mockProvider) SetupSharedFacet(SharedFacetParams)              {}
func (p *mockProvider) RestoreFacetSessions(RestoreParams)                  {}
func (p *mockProvider) SetLifecycleCallback(string, LifecycleEvent, func())    {}
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

// TestIsSilent covers the post-TrimSpace exact-match behaviour: empty and
// whitespace-only text, the [[NO_RESPONSE]] sentinel, and CC's synthetic
// "No response requested." message are silent; everything else is not.
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
		{"prefix_only", "[[NO_RESP", false},
		{"with_trailing_text", "[[NO_RESPONSE]] OK", false},
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
