package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/config"
	"foci/internal/delegator/accounting"
	"foci/internal/epdispatch"
	"foci/internal/fap"
	"foci/internal/fastload"
	"foci/internal/platform"
	"foci/internal/provider"
)

// Pin label summary (#2956). When the user pins a message, the app opens the
// pin label dialog and sends a pin.summarize; the server asks a small fast
// model for a summary of at most ten words and answers the asking socket with
// one pin.summary. The app puts it in the label field without confirming the
// dialog. The model, the endpoint it is called on and the OpenRouter provider
// order are [platforms.app] config (pin_summary_model, pin_summary_endpoint,
// pin_summary_provider) and apply live: ConfigurePinSummary is their applier.
//
// The same summariser serves every fast-summary task: the pin label, the
// Stop-view subagent summaries (#2960), the automatic chat name (#2962) and
// the chat sections (#2993). The chat name and the sections can have a model
// and endpoint of their own (auto_name_model / auto_name_endpoint,
// chat_sections_model / chat_sections_endpoint, #3012), for example a local
// model; an empty key takes the pin_summary_* value. Every call is recorded
// in fastload.Default, whose figures /status shows, so the frequency of the
// tasks can be tuned to what the model can take.

const (
	defaultPinSummaryModel    = "meta-llama/llama-3.1-8b-instruct"
	defaultPinSummaryProvider = "groq"
	// defaultPinSummaryEndpoint is the [endpoints] entry the model is called
	// on. When the config has no such entry (the built-in openrouter entry
	// exists only when a model group uses it), openRouterChatURL and
	// pinSummarySecret are used, as before #3012.
	defaultPinSummaryEndpoint = "openrouter"
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
	// pinLabelOwnTextWords: a message of fewer words than this is its own
	// pin label, with no model call (#3054, Dick 2026-11-11: "if the text is
	// less than 10 words, just use the text as the suggestion without
	// calling an API to summarise").
	pinLabelOwnTextWords = 10

	openRouterChatURL = "https://openrouter.ai/api/v1/chat/completions"
	pinSummarySecret  = "openrouter.api_key"
	// openAIDefaultBase is the base URL of an openai-format endpoint that sets
	// no URL (the SDK default).
	openAIDefaultBase = "https://api.openai.com/v1"

	// summaryReasoningEffort is the reasoning_effort every fast-summary call
	// sends (#3026). A thinking model (qwen3.8 on Ollama) otherwise spends the
	// whole small answer budget on reasoning and returns no answer. Ollama's
	// /v1 honours it (reasoning 0 tokens); OpenRouter takes it and ignores it
	// for a model that does not reason (Llama 3.1 8B on Groq, measured
	// 2026-11-10). An endpoint that refuses the field gets the call again
	// without it (reasoningFieldRefused).
	summaryReasoningEffort = "none"
)

// pinSummaryPrompt is the system prompt. Dick's ruling: "Prompt sets a strict
// ten word limit."
const pinSummaryPrompt = "You write short labels for saved chat messages. " +
	"Summarise the message the user sends in AT MOST TEN WORDS. This limit is strict: never more than ten words. " +
	"Reply with the summary only: no quotes, no preamble, no trailing full stop, one line."

// summaryUse picks which task's model and endpoint a call uses.
type summaryUse int

const (
	useShared   summaryUse = iota // pin_summary_* (the pin label, the Stop-view summaries)
	useAutoName                   // auto_name_model / auto_name_endpoint
	useSections                   // chat_sections_model / chat_sections_endpoint
)

// summaryTarget is where one task's calls go, resolved from config.
type summaryTarget struct {
	model     string   // the model id, sent as written; "" or pinSummaryOff = off
	endpoint  string   // the [endpoints] entry name
	providers []string // OpenRouter provider order; nil = OpenRouter picks
	// url is the chat-completions URL; "" = the summariser's openRouterURL
	// (the built-in OpenRouter endpoint).
	url string
	// secret names the endpoint's key; "" = the endpoint takes no key.
	secret string
	// openRouter: the endpoint is OpenRouter, so the provider order is sent.
	openRouter bool
	// timeout replaces the task's own timeout when > 0: the endpoint's
	// http_timeout, for an endpoint other than OpenRouter (a local model may
	// need far longer than the task's default).
	timeout time.Duration
	// problem is why the target cannot be called ("" = it can): an unknown
	// endpoint, or one that does not speak the OpenAI format.
	problem string
	// maxInFlight is the endpoint's max_in_flight (#3019): > 0 dispatches
	// the target's calls by priority (dispatcher).
	maxInFlight int
}

func (t summaryTarget) off() bool {
	return t.model == "" || strings.EqualFold(t.model, pinSummaryOff)
}

// label names the target in the log and the load figures.
func (t summaryTarget) label() string { return fastload.Target(t.model, t.endpoint) }

// pinSummariser holds the live settings and makes the model call. Settings
// are mutex-guarded so a live config edit (ConfigurePinSummary) swaps them
// under a request in flight safely.
type pinSummariser struct {
	mu      sync.RWMutex
	targets map[summaryUse]summaryTarget

	// openRouterURL is the chat-completions URL of the built-in OpenRouter
	// endpoint, used when the config has no [endpoints.openrouter]; a test
	// points it at a stub.
	openRouterURL string
	client        *http.Client                     // nil = http.DefaultClient
	secret        func(name string) (string, bool) // reads a secret at call time; nil = none
	load          *fastload.Recorder               // nil = fastload.Default

	// noEffort holds the targets (by label) whose endpoint refused
	// reasoning_effort (#3026): their calls leave it out. Guarded by mu;
	// configure clears it, since an edit can point the endpoint elsewhere.
	noEffort map[string]bool
}

// endpointsOf returns cfg's [endpoints] entries (nil for a nil cfg).
func endpointsOf(cfg *config.Config) map[string]config.EndpointConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Endpoints
}

// secretReader reads secrets from deps' store at call time, so a key set
// after startup is picked up; nil when the hub has no store.
func secretReader(deps platform.ProviderDeps) func(string) (string, bool) {
	if deps.SecretStore == nil {
		return nil
	}
	return deps.SecretStore.Get
}

func newPinSummariser(app *config.AppSpecific, endpoints map[string]config.EndpointConfig, secret func(string) (string, bool)) *pinSummariser {
	s := &pinSummariser{openRouterURL: openRouterChatURL, secret: secret}
	s.configure(app, endpoints)
	return s
}

// configure applies [platforms.app] pin_summary_model, pin_summary_endpoint,
// pin_summary_provider and the per-task model and endpoint keys, against the
// [endpoints] entries. A nil app or an empty field takes the code default (or
// the pin_summary_* value, for a per-task key), exactly as the platform
// default layer would.
func (s *pinSummariser) configure(app *config.AppSpecific, endpoints map[string]config.EndpointConfig) {
	model, prov, ep := defaultPinSummaryModel, defaultPinSummaryProvider, defaultPinSummaryEndpoint
	var nameModel, nameEP, secModel, secEP string
	if app != nil {
		if m := strings.TrimSpace(app.PinSummaryModel); m != "" {
			model = m
		}
		if p := strings.TrimSpace(app.PinSummaryProvider); p != "" {
			prov = p
		}
		if e := strings.TrimSpace(app.PinSummaryEndpoint); e != "" {
			ep = e
		}
		nameModel, nameEP = strings.TrimSpace(app.AutoNameModel), strings.TrimSpace(app.AutoNameEndpoint)
		secModel, secEP = strings.TrimSpace(app.ChatSectionsModel), strings.TrimSpace(app.ChatSectionsEndpoint)
	}
	var providers []string
	if !strings.EqualFold(prov, pinSummaryAnyProvider) {
		for _, p := range strings.Split(prov, ",") {
			if p = strings.TrimSpace(p); p != "" {
				providers = append(providers, p)
			}
		}
	}
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	targets := map[summaryUse]summaryTarget{
		useShared:   resolveSummaryTarget(model, ep, providers, endpoints),
		useAutoName: resolveSummaryTarget(or(nameModel, model), or(nameEP, ep), providers, endpoints),
		useSections: resolveSummaryTarget(or(secModel, model), or(secEP, ep), providers, endpoints),
	}
	s.mu.Lock()
	s.targets = targets
	s.noEffort = nil
	s.mu.Unlock()
}

// resolveSummaryTarget finds where model on endpoint is called. An endpoint
// absent from endpoints is an error, except the default (openrouter), which
// falls back to the built-in OpenRouter URL and key.
func resolveSummaryTarget(model, endpoint string, providers []string, endpoints map[string]config.EndpointConfig) summaryTarget {
	t := summaryTarget{model: model, endpoint: endpoint}
	ec, ok := endpoints[endpoint]
	if !ok {
		if endpoint == defaultPinSummaryEndpoint {
			t.secret, t.openRouter, t.providers = pinSummarySecret, true, providers
			return t
		}
		t.problem = fmt.Sprintf("endpoint %q is not in [endpoints]", endpoint)
		return t
	}
	if !ec.SupportsFormat("openai") {
		t.problem = fmt.Sprintf("endpoint %q does not speak the OpenAI chat format", endpoint)
		return t
	}
	base := ec.URLForFormat("openai")
	if base == "" {
		base = openAIDefaultBase
	}
	t.url = strings.TrimRight(base, "/") + "/chat/completions"
	t.secret = ec.APIKey
	t.maxInFlight = ec.MaxInFlight
	t.openRouter = endpoint == defaultPinSummaryEndpoint || strings.Contains(base, "openrouter.ai")
	if t.openRouter {
		t.providers = providers
	} else if d, err := time.ParseDuration(strings.TrimSpace(ec.HTTPTimeout)); err == nil && d > 0 {
		t.timeout = d
	}
	return t
}

// sendsEffort reports whether calls to t send reasoning_effort: true until
// its endpoint refuses the field.
func (s *pinSummariser) sendsEffort(t summaryTarget) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.noEffort[t.label()]
}

// dropEffort records that t's endpoint refused reasoning_effort.
func (s *pinSummariser) dropEffort(t summaryTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noEffort == nil {
		s.noEffort = map[string]bool{}
	}
	s.noEffort[t.label()] = true
}

func (s *pinSummariser) target(u summaryUse) summaryTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targets[u]
}

// key returns the target's key; ok is false when the endpoint needs a key
// that is not set.
func (s *pinSummariser) key(t summaryTarget) (string, bool) {
	if t.secret == "" {
		return "", true
	}
	if s.secret == nil {
		return "", false
	}
	k, _ := s.secret(t.secret)
	return k, k != ""
}

// usable reports whether a call for u can be made now: the model is not off,
// the endpoint is known and speaks the OpenAI format, and its key is set. A
// feature that runs unasked (the automatic chat name, #2962) checks it first,
// so it is on only when correctly configured.
func (s *pinSummariser) usable(u summaryUse) bool {
	if s == nil {
		return false
	}
	t := s.target(u)
	if t.off() || t.problem != "" {
		return false
	}
	_, ok := s.key(t)
	return ok
}

// fastSummaryWarnOnce makes the startup warning about a misconfigured fast
// summary path appear once per process, for all its users together (Dick,
// 2026-11-08: "only once overall for the shared rapid summary path").
var fastSummaryWarnOnce sync.Once

// warnIfUnconfigured logs, once per process, every fast-summary target that
// cannot run: its endpoint is unknown or not OpenAI-format, or its key is not
// set. Nothing is logged for a model set to off: that is a choice, not a
// fault.
func (s *pinSummariser) warnIfUnconfigured() {
	problems := s.problems()
	if len(problems) == 0 {
		return
	}
	fastSummaryWarnOnce.Do(func() {
		appLog.Warnf("fast summary: %s; those model calls are off (of pin label suggestions, Stop-view subagent summaries, "+
			"automatic chat names, chat sections)", strings.Join(problems, "; "))
	})
}

// problems lists, once each, why a target that is not off cannot be called.
func (s *pinSummariser) problems() []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range []summaryUse{useShared, useAutoName, useSections} {
		t := s.target(u)
		if t.off() {
			continue
		}
		p := t.problem
		if p == "" {
			if _, ok := s.key(t); !ok {
				p = fmt.Sprintf("secret %s (the key of endpoint %q) is not set", t.secret, t.endpoint)
			}
		}
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// ConfigurePinSummary is the live applier for [platforms.app]
// pin_summary_model, pin_summary_endpoint, pin_summary_provider,
// auto_name_model, auto_name_endpoint, chat_sections_model and
// chat_sections_endpoint, and for [endpoints] (cmd/foci-gw/liveapply.go). A
// no-op when the app provider is not running. A target that cannot be called
// after the edit is logged as a warning.
func ConfigurePinSummary(app *config.AppSpecific, endpoints map[string]config.EndpointConfig) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.pinSum == nil {
		return
	}
	h.pinSum.configure(app, endpoints)
	if p := h.pinSum.problems(); len(p) > 0 {
		appLog.Warnf("fast summary: %s; those model calls are off", strings.Join(p, "; "))
	}
}

// openRouterChatRequest is the subset of the OpenAI-compatible chat request
// OpenRouter takes that this call uses.
type openRouterChatRequest struct {
	Model       string                  `json:"model"`
	Messages    []openRouterChatMsg     `json:"messages"`
	MaxTokens   int                     `json:"max_tokens"`
	Temperature float64                 `json:"temperature"`
	Provider    *openRouterProviderPref `json:"provider,omitempty"`
	// ReasoningEffort asks a thinking model not to reason
	// (summaryReasoningEffort); "" = not sent.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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
			// Reasoning (Ollama, OpenRouter) or ReasoningContent (vLLM,
			// DeepSeek) is a thinking model's reasoning text; read only to
			// say why an answer is empty.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
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
	return s.ask(ctx, summaryAsk{what: "pin summary", system: pinSummaryPrompt, maxInputRunes: pinSummaryMaxInputRunes, maxTokens: pinSummaryMaxTokens, maxWords: pinSummaryMaxWords, timeout: pinSummaryTimeout}, text, session, agentID)
}

// summaryAsk is one kind of short summary the fast model writes: the pin
// label (#2956) or a subagent's current step (#2960).
type summaryAsk struct {
	what          string     // names the call in the log and the load figures
	use           summaryUse // whose model and endpoint the call goes to
	system        string     // the system prompt
	maxInputRunes int        // the text is cut to this many runes
	maxTokens     int        // the answer's token cap
	// timeout bounds the call (0 = only the caller's context). A target with
	// a timeout of its own (summaryTarget.timeout) uses that instead.
	timeout time.Duration
	// maxWords is the word count the prompt asks for (0 = no check). An
	// answer of more than 1.5 times this many words is not used: ask returns
	// an error and logs a warning (Dick, 2026-11-08: "Accept return up to
	// 1.5x requested length, if longer than that simply don't use the
	// result, and log warning"). A shorter answer is used as written, never
	// cut.
	maxWords int
	// raw returns the answer as the model wrote it, only trimmed: line breaks
	// are kept and it is not cleaned to one label line. For an answer in a
	// form its caller parses (the chat sections, #2993).
	raw bool
	// chat names the chat a Medium task's call is for (its conversation
	// id). On a dispatched endpoint (#3019) a waiting call of the same task
	// and chat is replaced by a newer one (dispatchKey).
	chat string
}

// dispatchTier is the priority of a's calls on a local endpoint (#3019): the
// pin label and the Stop-view summaries are High, like a chat turn (Dick's
// ruling Q59); the chat name and the sections are Medium.
func (a summaryAsk) dispatchTier() epdispatch.Tier {
	if a.use == useShared {
		return epdispatch.High
	}
	return epdispatch.Medium
}

// dispatchKey names a Medium call's task and chat for replacement ("" = no
// replacement).
func dispatchKey(what, chat string) string {
	if chat == "" {
		return ""
	}
	return what + "|" + chat
}

// dispatcher returns the endpoint's dispatcher when t's calls go through
// one: an [endpoints] entry with max_in_flight > 0 that is not OpenRouter
// (epdispatch.Engage). nil = the call goes straight out.
func (t summaryTarget) dispatcher() *epdispatch.Dispatcher {
	if t.openRouter || t.url == "" {
		return nil
	}
	return epdispatch.Engage(t.endpoint, t.maxInFlight, t.url)
}

// replaceWaiting replaces the call of task what for chat that waits on u's
// endpoint, if one does (#3019, Dick's ruling Q61: "Keep only the newest per
// chat (a newer check replaces an older held one)"). The chat's check loop
// calls it when a newer message arrives: the waiting check returns
// epdispatch.ErrReplaced, and the loop runs again on the newest messages.
func (s *pinSummariser) replaceWaiting(u summaryUse, what, chat string) {
	if s == nil {
		return
	}
	t := s.target(u)
	if t.off() || t.problem != "" {
		return
	}
	if d := t.dispatcher(); d != nil {
		d.Replace(dispatchKey(what, chat))
	}
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
	t := s.target(a.use)
	if t.off() {
		return "", nil
	}
	if t.problem != "" {
		return "", fmt.Errorf("%s", t.problem)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("no text to summarise")
	}
	key, ok := s.key(t)
	if !ok {
		return "", fmt.Errorf("secret %s is not set", t.secret)
	}
	load := s.load
	if load == nil {
		load = fastload.Default
	}
	// A local endpoint that sets max_in_flight takes a limited number of
	// foci's calls at a time, in priority order (#3019). The wait has no time
	// limit (Q61) and comes before the task's timeout, which bounds only the
	// call itself.
	if d := t.dispatcher(); d != nil {
		release, wait, err := d.Acquire(ctx, a.dispatchTier(), dispatchKey(a.what, a.chat))
		replaced := errors.Is(err, epdispatch.ErrReplaced)
		if wait > 0 || replaced {
			load.Held(t.label(), a.what, wait, replaced)
		}
		if err != nil {
			if replaced {
				appLog.Infof("%s: session=%s target=%s: a newer check of the chat replaced this one while it waited", a.what, session, t.label())
			}
			return "", err
		}
		defer release()
	}
	timeout := a.timeout
	if t.timeout > 0 {
		timeout = t.timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	url := t.url
	if url == "" {
		url = s.openRouterURL
	}
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	call := func(effort string) (openRouterChatResponse, error) {
		body := openRouterChatRequest{
			Model: t.model,
			Messages: []openRouterChatMsg{
				{Role: "system", Content: a.system},
				{Role: "user", Content: truncateRunes(text, a.maxInputRunes)},
			},
			MaxTokens:       a.maxTokens,
			Temperature:     0,
			ReasoningEffort: effort,
		}
		if t.openRouter && len(t.providers) > 0 {
			body.Provider = &openRouterProviderPref{Order: t.providers, AllowFallbacks: true}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return openRouterChatResponse{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return openRouterChatResponse{}, err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", "application/json")
		return s.post(client, req, t.endpoint)
	}

	ahead, done := load.Begin(t.label(), a.what)
	start := time.Now()
	var out openRouterChatResponse
	var err error
	if s.sendsEffort(t) {
		out, err = call(summaryReasoningEffort)
		if reasoningFieldRefused(err) {
			// The endpoint does not take the field (or its model must
			// reason): ask again without it, and leave it out from now on.
			appLog.Infof("%s: target=%s refused reasoning_effort, retrying without it from now on: %v", a.what, t.label(), err)
			s.dropEffort(t)
			out, err = call("")
		}
	} else {
		out, err = call("")
	}
	duration := time.Since(start)
	done(err != nil)
	if err != nil {
		appLog.Infof("%s: session=%s target=%s failed after %s (%d other calls in flight at start): %v",
			a.what, session, t.label(), duration.Round(time.Millisecond), ahead, err)
		return "", err
	}

	served := t.model
	if out.Model != "" {
		served = out.Model
	}
	appLog.Infof("%s: session=%s model=%s endpoint=%s input=%d output=%d finish=%s duration=%s in_flight_at_start=%d",
		a.what, session, served, t.endpoint, out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Choices[0].FinishReason, duration.Round(time.Millisecond), ahead)
	bookSummary(a.what, out, served, t.endpoint, session, agentID, start, duration)

	if a.raw {
		answer := strings.TrimSpace(stripThinkingPreamble(out.Choices[0].Message.Content))
		if answer == "" {
			return "", emptyAnswerError(t.endpoint, "empty answer", out, a.maxTokens)
		}
		return answer, nil
	}
	summary := cleanPinSummary(stripThinkingPreamble(out.Choices[0].Message.Content))
	if summary == "" {
		return "", emptyAnswerError(t.endpoint, "empty summary", out, a.maxTokens)
	}
	if !summaryWordsOK(summary, a.maxWords) {
		appLog.Warnf("%s: session=%s answer has %d words, more than 1.5x the %d asked for; not used: %q",
			a.what, session, len(strings.Fields(summary)), a.maxWords, truncateRunes(summary, 200))
		return "", fmt.Errorf("answer too long: %d words, limit %d", len(strings.Fields(summary)), a.maxWords)
	}
	return summary, nil
}

// emptyAnswerError is the error for a reply with no answer text. When the
// model stopped at the token cap (finish_reason length), it says so and how
// much reasoning it wrote: a thinking model that spent the budget reasoning
// (#3026) is then told apart from one that answered nothing.
func emptyAnswerError(endpoint, what string, out openRouterChatResponse, maxTokens int) error {
	c := out.Choices[0]
	if c.FinishReason != "length" {
		return fmt.Errorf("%s: %s", endpoint, what)
	}
	reasoning := utf8.RuneCountInString(c.Message.Reasoning) + utf8.RuneCountInString(c.Message.ReasoningContent)
	return fmt.Errorf("%s: %s: the model hit the %d-token answer cap (finish_reason=length, %d output tokens) after %d chars of reasoning",
		endpoint, what, maxTokens, out.Usage.CompletionTokens, reasoning)
}

// httpStatusError is a non-200 reply from a chat-completions endpoint.
type httpStatusError struct {
	endpoint string
	status   int
	body     string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.endpoint, e.status, truncateRunes(e.body, 300))
}

// reasoningFieldRefused reports whether err is an endpoint refusing the
// request because of the reasoning field: HTTP 400 or 422 whose body names
// reasoning. OpenAI and Groq refuse an argument a model does not take this
// way, and OpenRouter answers a model that must reason with 400 "Reasoning
// is mandatory for this endpoint and cannot be disabled" (measured
// 2026-11-10, openai/gpt-5-mini).
func reasoningFieldRefused(err error) bool {
	var he *httpStatusError
	if !errors.As(err, &he) {
		return false
	}
	if he.status != http.StatusBadRequest && he.status != http.StatusUnprocessableEntity {
		return false
	}
	return strings.Contains(strings.ToLower(he.body), "reasoning")
}

// post sends req and decodes a chat-completions answer with at least one
// choice; endpoint names the endpoint in errors.
func (s *pinSummariser) post(client *http.Client, req *http.Request, endpoint string) (openRouterChatResponse, error) {
	var out openRouterChatResponse
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, &httpStatusError{endpoint: endpoint, status: resp.StatusCode, body: string(respBody)}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	if out.Error != nil {
		return out, fmt.Errorf("%s: %s", endpoint, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return out, fmt.Errorf("%s: no choices", endpoint)
	}
	return out, nil
}

// bookSummary books the call in the cost ledger as a summary call, on a
// system turn of its own in the session it was made for. endpoint is the
// [endpoints] entry it went to: a zero_cost one books it at $0 (#3035).
func bookSummary(what string, out openRouterChatResponse, model, endpoint, session, agentID string, start time.Time, duration time.Duration) {
	turn := accounting.OwnTurn(
		accounting.MintTurnID(session, accounting.KindSummary, start),
		session, agentID, accounting.SourceSystem, start, start.Add(duration))
	usage := provider.Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	stop := ""
	if len(out.Choices) > 0 {
		stop = out.Choices[0].FinishReason
	}
	if err := accounting.Record(turn, accounting.APIResponse{
		ID: out.ID, Kind: accounting.KindSummary, Provider: "openai", Model: model, Endpoint: endpoint,
		Session: session, AgentID: agentID, TurnID: turn.TurnID,
		Start: start, Duration: duration, Tokens: usage.Tokens(), StopReason: stop,
	}.Call()); err != nil {
		appLog.Warnf("%s: book call: %v", what, err)
	}
}

// stripThinkingPreamble keeps only the text after the final closing think tag.
// Some local thinking models include their reasoning inline in message.content.
func stripThinkingPreamble(s string) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		return s[i+len("</think>"):]
	}
	return s
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
	if label, ok := shortPinLabel(f.Text); ok {
		// The feature turned off (pin_summary_model = "off") still means no
		// suggestion; otherwise a short message is its own label.
		if h.pinSum.target(useShared).off() {
			label = ""
		}
		client.sendRaw(fap.PinSummary{RequestID: f.RequestID, MessageID: f.MessageID, Summary: label})
		return
	}
	var session, agentID string
	if b := h.convForReliability(f.ConversationID); b != nil {
		session, agentID = b.key(), b.agent()
	}
	go func() {
		ctx, cancel := context.WithCancel(context.Background()) // ask applies the timeout
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

// shortPinLabel returns text as its own pin label when it has fewer than
// pinLabelOwnTextWords words (#3054): words are counted as strings.Fields
// counts them (runs of non-space), the same count summaryWordsOK uses, and
// the label is the text on one line, its whitespace runs collapsed to one
// space. Markdown is kept as written. ok is false for a longer or a blank
// text, which goes to the model as before.
func shortPinLabel(text string) (string, bool) {
	words := strings.Fields(text)
	if len(words) == 0 || len(words) >= pinLabelOwnTextWords {
		return "", false
	}
	return strings.Join(words, " "), true
}
