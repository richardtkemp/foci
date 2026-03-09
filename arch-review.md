# Foci Architecture Review

## Executive Summary

Foci is a well-structured Go application for AI agent orchestration with a clean dependency graph and strong separation of concerns. The architecture follows a layered approach with clear data flow boundaries. However, the current design has **tight coupling to Telegram** as the sole messaging platform, which would require significant refactoring to support additional platforms.

**Key findings:**
- Package dependencies are clean with no circular imports
- Provider abstraction (for LLM clients) is well-designed and could serve as a model
- Telegram coupling is deep and pervasive across multiple packages
- Several package relationships would benefit from interface extraction

---

## Package Dependency Analysis

### Current Dependency Graph

```
main
├── config (leaf)
├── log (leaf)
├── secrets (leaf)
│   └── bitwarden → log
├── provider (leaf - neutral types)
├── anthropic → provider
├── gemini → provider
├── openai → provider
├── session → provider, log
├── memory → session (indirect via store types)
├── voice → log
├── skills → log
├── startup → log, state
├── mcp → provider, log, tools
├── tools → provider, log, memory, secrets, voice, session, state
├── workspace → provider
├── compaction → provider, session, log
├── provision (leaf)
├── command → agent, session, workspace, config, state, provider
├── mana → anthropic, log
├── warnings → log
├── agent → provider, compaction, mana, warnings, session, tools, workspace, log
├── periodic → mana, warnings, config, log, memory, state
└── telegram → agent, command, log, session, state, voice
```

### Dependency Quality Assessment

**Strengths:**
- **No circular dependencies** - the graph is a clean DAG
- **Leaf packages are well-isolated** - `config`, `log`, `secrets`, `provider`, `skills`, `prompts`, `provision`, `mana`, `warnings`, `startup` have minimal dependencies
- **Provider abstraction is clean** - all LLM clients implement the same interface, translation happens at boundaries

**Areas for improvement:**
- `tools` package imports many other packages (memory, secrets, voice, session, state) - could benefit from further decomposition
- `command` imports `agent` which creates coupling that's worked around via callbacks
- `telegram` sits at the top of the dependency tree, importing both `agent` and `command`

---

## Package Coupling Analysis

### High Coupling Packages

#### 1. `internal/telegram` (Highest coupling)

The telegram package is the most coupled package in the system. It imports:
- `agent` - for message handling
- `command` - for slash command routing
- `session` - for session key management
- `state` - for persistence
- `voice` - for transcription/TTS

**Current structure:**
- `bot.go` (~1500 lines) - monolithic message handling, command dispatch, media processing
- `manager.go` - bot lifecycle management
- `pool.go` - multiball session management
- `stream_writer.go` - streaming output
- Various format helpers

**Issues:**
1. `Bot` struct has 50+ fields directly embedding Telegram-specific concepts
2. No interface abstraction for messaging platform operations
3. Tight binding to `gotgbot` library types throughout
4. Command dispatch and agent invocation are interleaved

#### 2. `internal/tools` (Wide coupling)

Imports: `config`, `display`, `log`, `memory`, `provider`, `secrets`, `bitwarden`, `session`, `state`, `voice`

**Current structure:** 80+ files organized by tool type

**Issues:**
1. The `TelegramSender` interface is defined in `tools/telegram.go` - this is a good pattern but incomplete
2. Tools directly reference concrete types from many packages
3. Some tools (e.g., `send_telegram`) are platform-specific with no abstraction

#### 3. `internal/command` (Callback coupling)

Imports: `agent`, `session`, `workspace`, `config`, `state`, `provider`, `tools`

**Current structure:** 27 files with command implementations

**Issues:**
1. Uses callback functions to avoid importing `telegram` (good pattern)
2. `AgentDeps` struct in `deps.go` is a "god struct" with 20+ fields
3. Commands are tightly bound to Telegram concepts (ChatID context keys)

---

## Interface Opportunities

### 1. Messaging Platform Interface (Critical for multi-platform support)

**Current state:** None exists

**Recommended interface:**

```go
// internal/platform/platform.go
package platform

type Message struct {
    ID        string
    Text      string
    SenderID  string
    ChatID    string
    Timestamp time.Time
    Media     []MediaAttachment
    ReplyTo   *string // message ID being replied to
}

type MediaAttachment struct {
    Type      string // "image", "video", "audio", "document"
    Data      []byte
    MimeType  string
    SavedPath string
}

type SendOptions struct {
    ParseMode   string // "markdown", "html", ""
    ReplyTo     string // message ID to reply to
    InlineKeyboard []KeyboardButton
}

type KeyboardButton struct {
    Text string
    Data string
}

// Platform is the interface for a messaging platform.
type Platform interface {
    // Message handling
    Receive(ctx context.Context) (<-chan Message, error)
    Send(ctx context.Context, chatID string, text string, opts SendOptions) error
    SendDocument(ctx context.Context, chatID string, path string) error
    SendTyping(ctx context.Context, chatID string) error
    
    // Callback/interaction handling
    OnCallback(ctx context.Context, handler func(callbackID string, data string) error)
    AnswerCallback(callbackID string, text string) error
    
    // Session management
    SessionKeyForChat(chatID string) string
    
    // Lifecycle
    Start(ctx context.Context) error
    Stop() error
    
    // Capabilities
    SupportsEditing() bool
    SupportsInlineKeyboard() bool
    MaxMessageLength() int
}

// Sender provides outbound messaging capabilities.
// This is what tools use to send messages.
type Sender interface {
    SendText(chatID string, text string) error
    SendDocument(chatID string, path string) error
    SendVoice(chatID string, path string) error
    // ... other media types
}
```

### 2. Tool Result Interface

**Current state:** `ToolResult` struct in `tools/registry.go`

**Issue:** Contains `ExtraBlocks []provider.ContentBlock` which ties tools to provider types

**Recommendation:** This is acceptable since tools are inherently model-facing, but document the coupling.

### 3. Command Context Interface

**Current state:** `AgentDeps` struct with many fields

**Recommendation:** Split into focused interfaces:

```go
// internal/command/context.go
type SessionContext interface {
    SessionKey() string
    ChatID() int64
    DefaultSessionKey() string
}

type AgentContext interface {
    Model() string
    IsProcessing() bool
    HandleMessage(ctx context.Context, sessionKey, text string) (string, error)
}

type StorageContext interface {
    StateStore() *state.Store
    Sessions() *session.Store
}
```

---

## Package Split/Combine Recommendations

### Split: `internal/telegram`

**Current:** Single large package with bot logic, formatting, pooling, streaming

**Recommendation:** Split into:

```
internal/telegram/
├── bot.go          # Core Bot struct and message handling
├── format.go       # Markdown/HTML conversion (already exists)
├── platform.go     # Platform interface implementation
├── pool.go         # Multiball pool logic
├── manager.go      # Bot lifecycle management
├── stream.go       # Streaming output (already stream_writer.go)
├── callbacks.go    # Callback query handling
└── media.go        # Media download/processing
```

**Better yet:** Create `internal/platform` and move platform-agnostic interfaces there:

```
internal/platform/
├── platform.go     # Platform interface
├── message.go      # Message types
└── sender.go       # Sender interface

internal/telegram/
├── bot.go          # telegram.Bot implementing platform.Platform
├── format.go       # Telegram-specific formatting
└── ...
```

### Split: `internal/tools`

**Current:** 80+ files in flat structure

**Recommendation:** Organize by capability:

```
internal/tools/
├── registry.go     # Core types (keep)
├── context.go      # Context helpers (keep)
├── exec/
│   ├── shell.go
│   ├── execbridge.go
│   └── procattr.go
├── files/
│   ├── files.go
│   ├── syntax.go
│   └── web.go
├── messaging/
│   ├── telegram.go    # send_telegram
│   └── session_send.go
├── memory/
│   ├── memory.go
│   ├── remind.go
│   └── scratchpad.go
├── process/
│   ├── tmux.go
│   ├── tmux_*.go
│   └── browser.go
└── external/
    ├── http.go
    ├── spawn.go
    └── summary.go
```

### Combine: Consider merging small packages

- `mana` and `warnings` are both small, leaf packages related to agent monitoring
- `skills` and `prompts` are both content-loading packages

However, both are well-isolated, so merging is low priority.

---

## Multi-Platform Support Analysis

### Current Telegram Coupling Points

| Component | Telegram Coupling | Difficulty to Abstract |
|-----------|-------------------|----------------------|
| `internal/telegram/bot.go` | Direct `gotgbot` types | High - 1500 lines |
| `internal/command/deps.go` | ChatIDKey context value | Low |
| `internal/tools/telegram.go` | `TelegramSender` interface | Already partially abstracted |
| `cmd/foci-gw/main.go` | Telegram bot creation | Medium |
| `cmd/foci-gw/agents.go` | `applyAgentDisplaySettings` | Medium |
| Session keys | `agent:ID:chat:CHATID` format | Low (format is opaque) |

### Required Changes for Multi-Platform

#### Phase 1: Extract Platform Interface

1. **Create `internal/platform` package** with interfaces:
   - `Platform` - inbound/outbound messaging
   - `Sender` - outbound only (for tools)
   - `Message` - platform-neutral message type
   - `Attachment` - platform-neutral media type

2. **Move `TelegramSender` interface** from `tools/telegram.go` to `platform/sender.go`

3. **Create Telegram implementation** of `Platform` interface:
   - `telegram.Bot` already has most methods
   - Need to adapt `gotgbot` types to platform-neutral types

#### Phase 2: Decouple Agent from Platform

Current coupling in `agent/agent.go`:
```go
// TurnCallbacks has Telegram-specific callbacks
type TurnCallbacks struct {
    ReplyFunc          func(text string)
    ToolCallObserver   func(toolName string, params json.RawMessage)
    ToolResultObserver func(toolName string, result string, isError bool)
    // ...
}
```

These callbacks are actually platform-agnostic! They're already good. The coupling is in:
- Session key format assumptions (minor - format is opaque string)
- Callback function signatures (already abstract)

#### Phase 3: Platform-Aware Tool Registration

Current `send_telegram` tool:
```go
func NewSendTelegramTool(getSender func(sessionKey string) TelegramSender, tts voice.TTS) *Tool
```

Should become:
```go
func NewSendMessageTool(getSender func(sessionKey string) platform.Sender, tts voice.TTS) *Tool
```

The tool can check capabilities:
```go
if sender, ok := sender.(platform.DocumentSender); ok {
    sender.SendDocument(chatID, path)
}
```

### Estimated Effort

| Task | Lines Changed | Effort |
|------|---------------|--------|
| Create platform interfaces | ~200 new | 1 day |
| Implement telegram.Platform | ~300 modified | 2 days |
| Update tools to use platform.Sender | ~100 modified | 1 day |
| Update main/agents wiring | ~200 modified | 1 day |
| Add config for platform selection | ~100 new | 0.5 days |
| Testing and edge cases | - | 2 days |
| **Total** | ~900 | **7-8 days** |

---

## Signal Protocol Implementation Analysis

### What is Signal Protocol?

Signal Protocol is a cryptographic protocol for end-to-end encrypted messaging. It is **not** Signal Messenger (the app) - it's the underlying encryption layer that can be implemented on any messaging platform. Key components:

- **Double Ratchet Algorithm** - provides forward secrecy; each message uses a new key
- **X3DH (Extended Triple Diffie-Hellman)** - initial authenticated key exchange
- **Sesame** - session management for multiple devices
- **Authenticated Encryption** - messages are both encrypted and integrity-protected

The question is: how could Foci implement Signal Protocol to add E2E encryption on top of any messaging platform (Telegram, Discord, Matrix, etc.)?

### Architecture for Signal Protocol Layer

Signal Protocol would sit between the agent and the messaging platform:

```
┌─────────────────────────────────────────────────────────────┐
│                         Foci Agent                          │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    Signal Protocol Layer                     │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐   │
│  │  Key Store   │  │ Session State│  │ Ratchet State    │   │
│  │  (Identity,  │  │ (Per-recipient│ │ (Sending/Receiving│  │
│  │   Prekeys)   │  │  sessions)   │  │  chains)         │   │
│  └──────────────┘  └──────────────┘  └──────────────────┘   │
│                                                              │
│  Encrypt(plaintext, recipient) → ciphertext                 │
│  Decrypt(ciphertext, sender) → plaintext                    │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                   Messaging Platform                         │
│         (Telegram, Discord, Matrix, HTTP, etc.)             │
└─────────────────────────────────────────────────────────────┘
```

### Library Availability

**Go Options:**

1. **`github.com/signalapp/libsignal-protocol-c-go`** (Unofficial bindings)
   - CGO bindings to the official C library
   - Incomplete, unmaintained
   - **Not recommended**

2. **`github.com/nickcabral/libsignal-protocol-go`** (Pure Go port)
   - Pure Go implementation
   - Incomplete, not production-ready
   - Missing X3DH, incomplete ratchet

3. **`github.com/Threema/threema-core`** (Reference implementation)
   - Pure Go, actively maintained
   - Used by Threema (Signal Protocol compatible)
   - Implements Double Ratchet, X3DH, signed prekeys
   - **Recommended starting point**

4. **Roll your own** (Not recommended)
   - Signal Protocol is cryptographically subtle
   - Easy to introduce vulnerabilities
   - 4-6 weeks minimum for correct implementation

### Implementation Challenges

#### 1. Identity Key Management

Each agent needs:
- Long-term identity key pair (Curve25519)
- Signed prekeys (rotated periodically)
- One-time prekeys (consumed on first contact)

```go
// internal/crypto/identity.go
type IdentityStore struct {
    identityKeyPair *IdentityKeyPair
    signedPrekeys   map[uint32]*SignedPrekey
    oneTimePrekeys  map[uint32]*Prekey
    registrationID  uint32
}
```

**Challenge:** Where to persist keys? 
- Option A: `state.json` (existing pattern)
- Option B: New `identity.db` (SQLite, more secure)
- Option C: Hardware-backed (TPM, YubiKey)

#### 2. Per-Recipient Session State

Signal Protocol maintains state for each conversation partner:

```go
// internal/crypto/session.go
type SessionStore struct {
    sessions map[string]*SessionState  // recipientID → state
}

type SessionState struct {
    rootKey           []byte
    sendingChainKey   []byte
    receivingChains   map[uint32]*Chain
    sendingRatchetPub *PublicKey
    counter           uint32
}
```

**Challenge:** Session state must be persisted atomically - corrupted state = lost messages.

#### 3. Key Exchange (Out-of-Band)

X3DH requires exchanging public keys before encrypted communication. Options:

1. **First message contains prekey bundle** - Insecure if first message is intercepted
2. **QR code/key exchange via separate channel** - More secure but UX friction
3. **Trust-on-first-use (TOFU)** - Accept first key, verify later

For Foci, TOFU is most practical:
```go
// On first message from new sender:
// 1. Generate ephemeral key
// 2. Complete X3DH with sender's prekey bundle
// 3. Store derived session
// 4. Optionally: show fingerprint for user verification
```

#### 4. Message Format

Encrypted messages need to carry metadata:

```go
type Envelope struct {
    Version         byte   // Protocol version
    RegistrationID  uint32 // Sender's registration ID
    EphemeralKey    []byte // Ephemeral public key (if new session)
    Counter         uint32 // Message number
    PreviousCounter uint32 // Previous chain length
    Ciphertext      []byte // Encrypted payload
    MAC             []byte // Message authentication code
}
```

**Encoding:** Base64 or protocol buffers. Must fit in platform message limits.

#### 5. Platform Constraints

| Platform | Max Message | Supports Binary | Notes |
|----------|-------------|-----------------|-------|
| Telegram | 4096 chars | Via entities | Need Base64 encoding |
| Discord | 2000 chars | No | More fragmentation |
| Matrix | 65536 chars | Via m.room.encrypted | Native Signal support |
| HTTP/WebSocket | Unlimited | Yes | Easiest |

### Implementation Approach

#### Phase 1: Crypto Layer (internal/crypto)

```go
// internal/crypto/crypto.go
package crypto

type SignalProtocol struct {
    identity   *IdentityStore
    sessions   *SessionStore
    prekeys    *PrekeyStore
}

func NewSignalProtocol(identityPath string) (*SignalProtocol, error)
func (s *SignalProtocol) Encrypt(plaintext []byte, recipient string) (*Envelope, error)
func (s *SignalProtocol) Decrypt(envelope *Envelope, sender string) ([]byte, error)
func (s *SignalProtocol) GetPrekeyBundle() (*PrekeyBundle, error)
func (s *SignalProtocol) ProcessPrekeyBundle(bundle *PrekeyBundle, sender string) error
```

#### Phase 2: Integration Layer (internal/platform/encrypted)

```go
// internal/platform/encrypted.go
type EncryptedPlatform struct {
    underlying Platform
    crypto     *crypto.SignalProtocol
    recipients map[string]string // platformUserID → cryptoIdentity
}

func (e *EncryptedPlatform) Send(ctx context.Context, chatID, text string, opts SendOptions) error {
    recipient := e.recipients[chatID]
    envelope, err := e.crypto.Encrypt([]byte(text), recipient)
    if err != nil {
        return err
    }
    // Encode envelope for transport
    encoded := encodeEnvelope(envelope)
    return e.underlying.Send(ctx, chatID, encoded, opts)
}

func (e *EncryptedPlatform) Receive(ctx context.Context) (<-chan Message, error) {
    rawCh, err := e.underlying.Receive(ctx)
    if err != nil {
        return nil, err
    }
    // Decrypt messages in pipeline
    ch := make(chan Message)
    go func() {
        for msg := range rawCh {
            envelope := decodeEnvelope(msg.Text)
            plaintext, err := e.crypto.Decrypt(envelope, msg.SenderID)
            if err != nil {
                // Handle: unknown sender, corrupt message, etc.
                continue
            }
            msg.Text = string(plaintext)
            ch <- msg
        }
    }()
    return ch, nil
}
```

#### Phase 3: Key Distribution

The hardest part is establishing initial trust. Options:

1. **Pre-shared keys via config:**
   ```toml
   [[agents.trusted_users]]
   user_id = "123456"
   identity_key = "base64..."
   ```

2. **First-message key exchange:**
   - Sender includes prekey bundle in first message
   - Receiver processes and establishes session
   - Vulnerable to MITM but practical

3. **Side-channel verification:**
   - Display fingerprint in Telegram
   - User verifies out-of-band (in person, signal call, etc.)

### Required Changes

| Component | Change |
|-----------|--------|
| `internal/crypto/` | New package - Signal Protocol implementation |
| `internal/platform/encrypted.go` | Wrapper for encrypted messaging |
| `internal/config` | Identity key configuration |
| `internal/state` | Persist session state per recipient |
| `cmd/foci-gw/main.go` | Wire encrypted platform wrapper |

### Estimated Effort

| Task | Effort |
|------|--------|
| Integrate threema-core or similar | 2-3 days |
| Implement identity/session stores | 2 days |
| Build encrypted platform wrapper | 2 days |
| Key exchange flow (TOFU) | 1-2 days |
| Config and wiring | 1 day |
| Testing with real keys | 2 days |
| Edge cases (key rotation, lost state) | 2 days |
| **Total** | **12-15 days** |

### Security Considerations

1. **Forward secrecy** - Achieved via Double Ratchet; old keys deleted
2. **Compromised state** - If session state is stolen, attacker can decrypt future messages until ratchet advances
3. **Key backup** - No key backup = lost history on device loss
4. **Metadata** - Signal Protocol doesn't hide message metadata (timestamps, sender/recipient)
5. **Platform trust** - The messaging platform still sees encrypted blobs and metadata

### Alternative: Matrix (Built-in Signal Protocol)

Matrix natively supports Signal Protocol via `m.room.encrypted` events. If implementing on Matrix:
- No crypto code needed in Foci
- Matrix homeserver handles key management
- Foci just sends/receives via Matrix API
- **Effort:** 3-5 days for Matrix platform implementation

This is the path of least resistance for encrypted messaging.

---

## Recommendations

### Priority 1: ✅ DONE - Extract Platform Interface (Enables multi-platform)

**Completed in commits:**
- `2f8a73f4` - feat(platform): add Platform and Sender interface types
- `a45d4ca5` - feat(command): add platform-agnostic Request/Response types
- `0668c3ad` - feat(telegram): add platform-aware command dispatcher
- `c89801c1` - feat(telegram): implement platform.Sender interface
- `bedb36df` - feat(tools): use platform.Sender for send_telegram tool

**What's now in place:**
- `internal/platform/types.go` defines `Sender`, `Platform`, `Message`, `Request`, `Response`
- `telegram.Bot` implements `platform.Sender`
- `telegram.Dispatcher` provides platform-aware command dispatch
- `command.Registry.DispatchV2()` supports platform-agnostic command execution
- `tools.send_telegram` uses `platform.Sender` interface

**Impact:** Discord, Matrix, Slack, etc. can now be added by implementing `platform.Sender` and creating a platform-specific dispatcher.

### Priority 2: Reorganize `internal/tools` (Improves maintainability)

1. Group tools by capability in subdirectories
2. Each subdirectory can be understood independently
3. Reduces cognitive load when adding new tools

### Priority 3: ✅ PARTIAL - Split `internal/telegram` (Improves clarity)

**Completed:**
- Command dispatch extracted to `telegram/dispatch.go`
- Bot implements `platform.Sender` interface

**Remaining:**
- Consider moving platform-agnostic formatting to `internal/platform/format.go`
- Separate streaming, pooling, and callback logic into focused files

### Priority 4: Consider `internal/command` refactoring (Lower priority)

The callback pattern works well. `AgentDeps` renamed to `Deps` but still large. Only refactor further if adding new platforms reveals friction.

---

## Conclusion

Foci's architecture now has multi-platform support infrastructure in place:

**✅ Completed:**
1. **Platform interface extracted** - `internal/platform` defines messaging abstractions
2. **Telegram implements platform** - `Bot` implements `platform.Sender`
3. **Command dispatch refactored** - Platform-agnostic `Request`/`Response` pattern
4. **Tools use platform interface** - `send_telegram` uses `platform.Sender`

**What's next for new platforms:**
1. Implement `platform.Sender` interface (or full `platform.Platform`)
2. Create platform-specific dispatcher (like `telegram/dispatch.go`)
3. Wire up in main.go alongside or instead of Telegram
4. Migrate commands to `ExecuteV2` for full platform independence

For **Signal Protocol** (E2E encryption layer), the path forward is:

1. **Integrate an existing Go implementation** (threema-core recommended)
2. **Build an encrypted platform wrapper** that sits between agent and messaging platform
3. **Handle key exchange via TOFU or config-based pre-sharing**
4. **Persist session state carefully** - cryptographic state must be atomic

Signal Protocol adds ~12-15 days of work on top of the platform interface extraction. The main complexity is key management, not the crypto itself.

Alternatively, **Matrix provides native Signal Protocol support** - implementing a Matrix platform would give E2E encryption "for free" with ~5 days of work.

The codebase is well-positioned for both multi-platform support and cryptographic privacy with focused architectural work.
