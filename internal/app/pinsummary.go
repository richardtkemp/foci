package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/config"
	"foci/internal/delegator/accounting"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/provider"
)

// Pin label summary (#2956). When the user pins a message, the app opens the
// pin label dialog and sends a pin.summarize; the server asks a small fast
// model on OpenRouter for a summary of at most ten words and answers the
// asking socket with one pin.summary. The app puts it in the label field
// without confirming the dialog. The model and the OpenRouter provider order
// are [platforms.app] config (pin_summary_model, pin_summary_provider) and
// apply live: ConfigurePinSummary is their applier.

const (
	defaultPinSummaryModel    = "meta-llama/llama-3.1-8b-instruct"
	defaultPinSummaryProvider = "groq"
	// pinSummaryOff as the model turns the feature off: every request is
	// answered at once with an empty summary.
	pinSummaryOff = "off"
	// pinSummaryAnyProvider as the provider sends no provider preference, so
	// OpenRouter picks.
	pinSummaryAnyProvider = "any"

	// pinSummaryMaxInputRunes caps the message text sent to the model: a long
	// message is summarised from its start (clutch's default on the ticket).
	pinSummaryMaxInputRunes = 2000
	// pinSummaryTimeout bounds the whole model call. The app keeps the dialog
	// usable meanwhile; this only stops a hung call from holding a goroutine.
	pinSummaryTimeout = 15 * time.Second
	// pinSummaryMaxTokens is ample for ten words and stops a runaway answer.
	pinSummaryMaxTokens = 40
	// pinSummaryMaxWords is the limit the prompt states (Dick: "a strict ten
	// word limit"); summaryWordsOK accepts up to 1.5 times it.
	pinSummaryMaxWords = 10

	openRouterChatURL = "https://openrouter.ai/api/v1/chat/completions"
	pinSummarySecret  = "openrouter.api_key"
)

// pinSummaryPrompt is the system prompt. Dick's ruling: "Prompt sets a strict
// ten word limit."
const pinSummaryPrompt = "You write short labels for saved chat messages. " +
	"Summarise the message the user sends in AT MOST TEN WORDS. This limit is strict: never more than ten words. " +
	"Reply with the summary only: no quotes, no preamble, no trailing full stop, one line."

// pinSummariser holds the live settings and makes the model call. Settings
// are mutex-guarded so a live config edit (ConfigurePinSummary) swaps them
// under a request in flight safely.
type pinSummariser struct {
	mu        sync.RWMutex
	model     string   // OpenRouter model id; "" or pinSummaryOff = off
	providers []string // OpenRouter provider order; nil = OpenRouter picks

	url    string                // chat-completions endpoint; a test points it at a stub
	client *http.Client          // nil = http.DefaultClient
	apiKey func() (string, bool) // the OpenRouter key; nil = none
}

// secretGetter reads one secret from deps' store at call time, so a key set
// after startup is picked up; nil when the hub has no store.
func secretGetter(deps platform.ProviderDeps, name string) func() (string, bool) {
	if deps.SecretStore == nil {
		return nil
	}
	return func() (string, bool) { return deps.SecretStore.Get(name) }
}

func newPinSummariser(app *config.AppSpecific, apiKey func() (string, bool)) *pinSummariser {
	s := &pinSummariser{url: openRouterChatURL, apiKey: apiKey}
	s.configure(app)
	return s
}

// configure applies [platforms.app] pin_summary_model / pin_summary_provider.
// A nil app or an empty field takes the code default, exactly as the platform
// default layer would.
func (s *pinSummariser) configure(app *config.AppSpecific) {
	model, prov := defaultPinSummaryModel, defaultPinSummaryProvider
	if app != nil {
		if m := strings.TrimSpace(app.PinSummaryModel); m != "" {
			model = m
		}
		if p := strings.TrimSpace(app.PinSummaryProvider); p != "" {
			prov = p
		}
	}
	var providers []string
	if !strings.EqualFold(prov, pinSummaryAnyProvider) {
		for _, p := range strings.Split(prov, ",") {
			if p = strings.TrimSpace(p); p != "" {
				providers = append(providers, p)
			}
		}
	}
	s.mu.Lock()
	s.model, s.providers = model, providers
	s.mu.Unlock()
}

// usable reports whether a call can be made now: the model is not off and
// the OpenRouter key is set. A feature that runs unasked (the automatic chat
// name, #2962) checks it first, so it is on only when correctly configured.
func (s *pinSummariser) usable() bool {
	if s == nil {
		return false
	}
	model, _ := s.settings()
	if model == "" || strings.EqualFold(model, pinSummaryOff) {
		return false
	}
	return s.hasKey()
}

func (s *pinSummariser) hasKey() bool {
	if s.apiKey == nil {
		return false
	}
	key, _ := s.apiKey()
	return key != ""
}

// fastSummaryWarnOnce makes the startup warning about a misconfigured fast
// summary path appear once per process, for all its users together (Dick,
// 2026-11-08: "only once overall for the shared rapid summary path").
var fastSummaryWarnOnce sync.Once

// warnIfUnconfigured logs, once per process, that the fast summary path
// cannot run because the OpenRouter key is missing. Nothing is logged when
// the model is set to off: that is a choice, not a fault.
func (s *pinSummariser) warnIfUnconfigured() {
	model, _ := s.settings()
	if model == "" || strings.EqualFold(model, pinSummaryOff) || s.hasKey() {
		return
	}
	fastSummaryWarnOnce.Do(func() {
		appLog.Warnf("fast summary: secret %s is not set, so the model calls of [platforms.app] pin_summary_model are off: "+
			"pin label suggestions, Stop-view subagent summaries and automatic chat names", pinSummarySecret)
	})
}

func (s *pinSummariser) settings() (string, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model, s.providers
}

// ConfigurePinSummary is the live applier for [platforms.app]
// pin_summary_model and pin_summary_provider (cmd/foci-gw/liveapply.go). A
// no-op when the app provider is not running.
func ConfigurePinSummary(app *config.AppSpecific) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.pinSum == nil {
		return
	}
	h.pinSum.configure(app)
}

// openRouterChatRequest is the subset of the OpenAI-compatible chat request
// OpenRouter takes that this call uses.
type openRouterChatRequest struct {
	Model       string                  `json:"model"`
	Messages    []openRouterChatMsg     `json:"messages"`
	MaxTokens   int                     `json:"max_tokens"`
	Temperature float64                 `json:"temperature"`
	Provider    *openRouterProviderPref `json:"provider,omitempty"`
}

type openRouterChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openRouterProviderPref sends the call to the named providers first. With
// allow_fallbacks OpenRouter still tries others when they all fail, so a
// provider outage costs speed, not the summary.
type openRouterProviderPref struct {
	Order          []string `json:"order"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

type openRouterChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// summarise returns the model's cleaned summary of text. A "" summary with a
// nil error means the feature is off. session and agentID name where the call
// is booked in the cost ledger ("" = no session).
func (s *pinSummariser) summarise(ctx context.Context, text, session, agentID string) (string, error) {
	return s.ask(ctx, summaryAsk{what: "pin summary", system: pinSummaryPrompt, maxInputRunes: pinSummaryMaxInputRunes, maxTokens: pinSummaryMaxTokens, maxWords: pinSummaryMaxWords}, text, session, agentID)
}

// summaryAsk is one kind of short summary the fast model writes: the pin
// label (#2956) or a subagent's current step (#2960).
type summaryAsk struct {
	what          string // names the call in the log
	system        string // the system prompt
	maxInputRunes int    // the text is cut to this many runes
	maxTokens     int    // the answer's token cap
	// maxWords is the word count the prompt asks for (0 = no check). An
	// answer of more than 1.5 times this many words is not used: ask returns
	// an error and logs a warning (Dick, 2026-11-08: "Accept return up to
	// 1.5x requested length, if longer than that simply don't use the
	// result, and log warning"). A shorter answer is used as written, never
	// cut.
	maxWords int
}

// summaryWordsOK reports whether summary is within 1.5 times maxWords words
// (summaryAsk.maxWords). maxWords <= 0 accepts anything.
func summaryWordsOK(summary string, maxWords int) bool {
	if maxWords <= 0 {
		return true
	}
	return 2*len(strings.Fields(summary)) <= 3*maxWords
}

// ask returns the model's cleaned answer to text under a's system prompt. A ""
// answer with a nil error means the feature is off. session and agentID name
// where the call is booked in the cost ledger ("" = no session).
func (s *pinSummariser) ask(ctx context.Context, a summaryAsk, text, session, agentID string) (string, error) {
	model, providers := s.settings()
	if model == "" || strings.EqualFold(model, pinSummaryOff) {
		return "", nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("no text to summarise")
	}
	var key string
	if s.apiKey != nil {
		key, _ = s.apiKey()
	}
	if key == "" {
		return "", fmt.Errorf("secret %s is not set", pinSummarySecret)
	}

	body := openRouterChatRequest{
		Model: model,
		Messages: []openRouterChatMsg{
			{Role: "system", Content: a.system},
			{Role: "user", Content: truncateRunes(text, a.maxInputRunes)},
		},
		MaxTokens:   a.maxTokens,
		Temperature: 0,
	}
	if len(providers) > 0 {
		body.Provider = &openRouterProviderPref{Order: providers, AllowFallbacks: true}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	duration := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter HTTP %d: %s", resp.StatusCode, truncateRunes(string(respBody), 300))
	}
	var out openRouterChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode openrouter response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("openrouter: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openrouter: no choices")
	}

	served := model
	if out.Model != "" {
		served = out.Model
	}
	appLog.Infof("%s: session=%s model=%s input=%d output=%d duration=%s",
		a.what, session, served, out.Usage.PromptTokens, out.Usage.CompletionTokens, duration.Round(time.Millisecond))
	bookSummary(a.what, out, served, session, agentID, start, duration)

	summary := cleanPinSummary(out.Choices[0].Message.Content)
	if summary == "" {
		return "", fmt.Errorf("openrouter: empty summary")
	}
	if !summaryWordsOK(summary, a.maxWords) {
		appLog.Warnf("%s: session=%s answer has %d words, more than 1.5x the %d asked for; not used: %q",
			a.what, session, len(strings.Fields(summary)), a.maxWords, truncateRunes(summary, 200))
		return "", fmt.Errorf("answer too long: %d words, limit %d", len(strings.Fields(summary)), a.maxWords)
	}
	return summary, nil
}

// bookSummary books the call in the cost ledger as a summary call, on a
// system turn of its own in the session it was made for.
func bookSummary(what string, out openRouterChatResponse, model, session, agentID string, start time.Time, duration time.Duration) {
	turn := accounting.OwnTurn(
		accounting.MintTurnID(session, accounting.KindSummary, start),
		session, agentID, accounting.SourceSystem, start, start.Add(duration))
	usage := provider.Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	stop := ""
	if len(out.Choices) > 0 {
		stop = out.Choices[0].FinishReason
	}
	if err := accounting.Record(turn, accounting.APIResponse{
		ID: out.ID, Kind: accounting.KindSummary, Provider: "openai", Model: model,
		Session: session, AgentID: agentID, TurnID: turn.TurnID,
		Start: start, Duration: duration, Tokens: usage.Tokens(), StopReason: stop,
	}.Call()); err != nil {
		appLog.Warnf("%s: book call: %v", what, err)
	}
}

// cleanPinSummary makes the model's answer one label line: whitespace and
// line breaks collapse to single spaces, then surrounding quotes and a
// trailing full stop are removed (clutch's default on the ticket). The words
// are otherwise used as the model wrote them.
func cleanPinSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for {
		before := s
		s = strings.TrimSpace(s)
		s = trimPair(s, `"`, `"`)
		s = trimPair(s, `'`, `'`)
		s = trimPair(s, "“", "”")
		s = trimPair(s, "‘", "’")
		s = trimPair(s, "`", "`")
		if strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "..") { // a full stop, not an ellipsis
			s = strings.TrimSuffix(s, ".")
		}
		if s == before {
			break
		}
	}
	return s
}

func trimPair(s, open, close string) string {
	if len(s) >= len(open)+len(close) && strings.HasPrefix(s, open) && strings.HasSuffix(s, close) {
		return s[len(open) : len(s)-len(close)]
	}
	return s
}

// truncateRunes cuts s to at most n runes, on a rune boundary.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// handlePinSummarize answers one pin.summarize (#2956) on the asking socket.
// The model call runs off the socket's read loop, and is cancelled when the
// socket closes. Every request gets exactly one answer, an empty summary on
// failure or when the feature is off, so the app stops waiting.
func (h *Hub) handlePinSummarize(client *wsClient, f fap.PinSummarize) {
	if h.pinSum == nil {
		client.sendRaw(fap.PinSummary{RequestID: f.RequestID, MessageID: f.MessageID})
		return
	}
	var session, agentID string
	if b := h.convForReliability(f.ConversationID); b != nil {
		session, agentID = b.key(), b.agent()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pinSummaryTimeout)
		defer cancel()
		if client.done != nil {
			go func() {
				select {
				case <-client.done: // the socket closed: nobody to answer
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		summary, err := h.pinSum.summarise(ctx, f.Text, session, agentID)
		if err != nil {
			appLog.Warnf("pin summary %s/%s: %v", f.ConversationID, f.MessageID, err)
			summary = ""
		}
		client.sendRaw(fap.PinSummary{RequestID: f.RequestID, MessageID: f.MessageID, Summary: summary})
	}()
}
