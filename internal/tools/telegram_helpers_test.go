package tools

// mockTelegramSender records calls to all send methods.
type mockTelegramSender struct {
	sessionKey    string
	textCalls     []string
	documentCalls []string
	voiceCalls    []string
	videoCalls     []string
	photoCalls     []string
	audioCalls     []string
	animationCalls   []string
	voiceDataCalls   [][]byte
	textErr          error
	documentErr      error
	voiceErr         error
	videoErr         error
	photoErr         error
	audioErr         error
	animationErr     error
	voiceDataErr     error

	// Chat-targeted calls
	chatTextCalls      []mockChatCall
	chatDocumentCalls  []mockChatCall
	chatVoiceCalls     []mockChatCall
	chatVideoCalls     []mockChatCall
	chatPhotoCalls     []mockChatCall
	chatAudioCalls     []mockChatCall
	chatAnimationCalls []mockChatCall
	chatVoiceDataCalls []mockChatDataCall
}

func (m *mockTelegramSender) SessionKey() string {
	return m.sessionKey
}

type mockChatCall struct {
	chatID int64
	value  string // text or filePath
}

type mockChatDataCall struct {
	chatID int64
	data   []byte
}

func (m *mockTelegramSender) SendInjected(text string) error {
	m.textCalls = append(m.textCalls, text)
	return m.textErr
}

func (m *mockTelegramSender) SendDocument(filePath string) error {
	m.documentCalls = append(m.documentCalls, filePath)
	return m.documentErr
}

func (m *mockTelegramSender) SendVoice(filePath string) error {
	m.voiceCalls = append(m.voiceCalls, filePath)
	return m.voiceErr
}

func (m *mockTelegramSender) SendVideo(filePath string) error {
	m.videoCalls = append(m.videoCalls, filePath)
	return m.videoErr
}

func (m *mockTelegramSender) SendPhoto(filePath string) error {
	m.photoCalls = append(m.photoCalls, filePath)
	return m.photoErr
}

func (m *mockTelegramSender) SendAudio(filePath string) error {
	m.audioCalls = append(m.audioCalls, filePath)
	return m.audioErr
}

func (m *mockTelegramSender) SendAnimation(filePath string) error {
	m.animationCalls = append(m.animationCalls, filePath)
	return m.animationErr
}

func (m *mockTelegramSender) SendInjectedToChat(chatID int64, text string) error {
	m.chatTextCalls = append(m.chatTextCalls, mockChatCall{chatID, text})
	return m.textErr
}

func (m *mockTelegramSender) SendDocumentToChat(chatID int64, filePath string) error {
	m.chatDocumentCalls = append(m.chatDocumentCalls, mockChatCall{chatID, filePath})
	return m.documentErr
}

func (m *mockTelegramSender) SendVoiceToChat(chatID int64, filePath string) error {
	m.chatVoiceCalls = append(m.chatVoiceCalls, mockChatCall{chatID, filePath})
	return m.voiceErr
}

func (m *mockTelegramSender) SendVideoToChat(chatID int64, filePath string) error {
	m.chatVideoCalls = append(m.chatVideoCalls, mockChatCall{chatID, filePath})
	return m.videoErr
}

func (m *mockTelegramSender) SendPhotoToChat(chatID int64, filePath string) error {
	m.chatPhotoCalls = append(m.chatPhotoCalls, mockChatCall{chatID, filePath})
	return m.photoErr
}

func (m *mockTelegramSender) SendAudioToChat(chatID int64, filePath string) error {
	m.chatAudioCalls = append(m.chatAudioCalls, mockChatCall{chatID, filePath})
	return m.audioErr
}

func (m *mockTelegramSender) SendAnimationToChat(chatID int64, filePath string) error {
	m.chatAnimationCalls = append(m.chatAnimationCalls, mockChatCall{chatID, filePath})
	return m.animationErr
}

func (m *mockTelegramSender) SendVoiceData(audioData []byte) error {
	m.voiceDataCalls = append(m.voiceDataCalls, audioData)
	return m.voiceDataErr
}

func (m *mockTelegramSender) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	m.chatVoiceDataCalls = append(m.chatVoiceDataCalls, mockChatDataCall{chatID, audioData})
	return m.voiceDataErr
}
