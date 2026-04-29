package tools

// mockSender records calls to all send methods.
type mockSender struct {
	sessionKey     string
	textCalls      []string
	documentCalls  []string
	voiceCalls     []string
	videoCalls     []string
	photoCalls     []string
	audioCalls     []string
	animationCalls []string
	voiceDataCalls [][]byte

	// Captions captured alongside file paths (parallel to *Calls slices).
	// Empty string means no caption was passed.
	documentCaptions  []string
	videoCaptions     []string
	photoCaptions     []string
	audioCaptions     []string
	animationCaptions []string

	textErr      error
	documentErr  error
	voiceErr     error
	videoErr     error
	photoErr     error
	audioErr     error
	animationErr error
	voiceDataErr error

	// Chat-targeted calls. mockChatCall.caption captures caption when non-empty.
	chatTextCalls      []mockChatCall
	chatDocumentCalls  []mockChatCall
	chatVoiceCalls     []mockChatCall
	chatVideoCalls     []mockChatCall
	chatPhotoCalls     []mockChatCall
	chatAudioCalls     []mockChatCall
	chatAnimationCalls []mockChatCall
	chatVoiceDataCalls []mockChatDataCall
}

func (m *mockSender) SessionKey() string {
	return m.sessionKey
}

type mockChatCall struct {
	chatID  int64
	value   string // text or filePath
	caption string // empty for non-captioned methods (text, voice, voiceData)
}

type mockChatDataCall struct {
	chatID int64
	data   []byte
}

func (m *mockSender) SendText(text string) error {
	m.textCalls = append(m.textCalls, text)
	return m.textErr
}

func (m *mockSender) SendDocument(filePath, caption string) error {
	m.documentCalls = append(m.documentCalls, filePath)
	m.documentCaptions = append(m.documentCaptions, caption)
	return m.documentErr
}

func (m *mockSender) SendVoice(filePath string) error {
	m.voiceCalls = append(m.voiceCalls, filePath)
	return m.voiceErr
}

func (m *mockSender) SendVideo(filePath, caption string) error {
	m.videoCalls = append(m.videoCalls, filePath)
	m.videoCaptions = append(m.videoCaptions, caption)
	return m.videoErr
}

func (m *mockSender) SendPhoto(filePath, caption string) error {
	m.photoCalls = append(m.photoCalls, filePath)
	m.photoCaptions = append(m.photoCaptions, caption)
	return m.photoErr
}

func (m *mockSender) SendAudio(filePath, caption string) error {
	m.audioCalls = append(m.audioCalls, filePath)
	m.audioCaptions = append(m.audioCaptions, caption)
	return m.audioErr
}

func (m *mockSender) SendAnimation(filePath, caption string) error {
	m.animationCalls = append(m.animationCalls, filePath)
	m.animationCaptions = append(m.animationCaptions, caption)
	return m.animationErr
}

func (m *mockSender) SendTextToChat(chatID int64, text string) error {
	m.chatTextCalls = append(m.chatTextCalls, mockChatCall{chatID: chatID, value: text})
	return m.textErr
}

func (m *mockSender) SendDocumentToChat(chatID int64, filePath, caption string) error {
	m.chatDocumentCalls = append(m.chatDocumentCalls, mockChatCall{chatID: chatID, value: filePath, caption: caption})
	return m.documentErr
}

func (m *mockSender) SendVoiceToChat(chatID int64, filePath string) error {
	m.chatVoiceCalls = append(m.chatVoiceCalls, mockChatCall{chatID: chatID, value: filePath})
	return m.voiceErr
}

func (m *mockSender) SendVideoToChat(chatID int64, filePath, caption string) error {
	m.chatVideoCalls = append(m.chatVideoCalls, mockChatCall{chatID: chatID, value: filePath, caption: caption})
	return m.videoErr
}

func (m *mockSender) SendPhotoToChat(chatID int64, filePath, caption string) error {
	m.chatPhotoCalls = append(m.chatPhotoCalls, mockChatCall{chatID: chatID, value: filePath, caption: caption})
	return m.photoErr
}

func (m *mockSender) SendAudioToChat(chatID int64, filePath, caption string) error {
	m.chatAudioCalls = append(m.chatAudioCalls, mockChatCall{chatID: chatID, value: filePath, caption: caption})
	return m.audioErr
}

func (m *mockSender) SendAnimationToChat(chatID int64, filePath, caption string) error {
	m.chatAnimationCalls = append(m.chatAnimationCalls, mockChatCall{chatID: chatID, value: filePath, caption: caption})
	return m.animationErr
}

func (m *mockSender) SendVoiceData(audioData []byte) error {
	m.voiceDataCalls = append(m.voiceDataCalls, audioData)
	return m.voiceDataErr
}

func (m *mockSender) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	m.chatVoiceDataCalls = append(m.chatVoiceDataCalls, mockChatDataCall{chatID, audioData})
	return m.voiceDataErr
}
