package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// The system prompt is session-scoped, not per-turn: a delegated backend
// receives it once at launch, the API path rebuilds it per call but it only
// changes when files on disk do. So it is registered by whoever assembles it
// (DelegatedManager.Get at launch; the API transport after
// BuildSystemAndTools) and each turn's Complete records the hash — plus the
// full text once per session per distinct hash, so a character-file edit
// shows up as a new system_prompt event on the first turn after it.

type systemPrompt struct {
	hash   string
	text   string
	source string
}

var (
	spMu       sync.Mutex
	spCurrent  = map[string]systemPrompt{} // session → prompt in force
	spExported = map[string]string{}       // session → hash last exported in full
)

// RegisterSystemPrompt records the system prompt now in force for session.
// source names the assembler ("launch" for a delegated backend start, "api"
// for a direct-API request). No-op when tracing is off.
func RegisterSystemPrompt(session, text, source string) {
	if !Enabled() || session == "" || text == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	spMu.Lock()
	spCurrent[session] = systemPrompt{hash: hex.EncodeToString(sum[:]), text: text, source: source}
	spMu.Unlock()
}

func systemPromptFor(session string) *systemPrompt {
	spMu.Lock()
	defer spMu.Unlock()
	sp, ok := spCurrent[session]
	if !ok {
		return nil
	}
	return &sp
}

// markSystemPromptExported reports whether hash still needs exporting in full
// for session, and records that it now has been.
func markSystemPromptExported(session, hash string) bool {
	spMu.Lock()
	defer spMu.Unlock()
	if spExported[session] == hash {
		return false
	}
	spExported[session] = hash
	return true
}
