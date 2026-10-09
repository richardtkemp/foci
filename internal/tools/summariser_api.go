package tools

import (
	"context"
	"fmt"
	"time"

	"foci/internal/config"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/session"
)

// APISummariser implements Summariser by calling provider.Send directly.
// Used by API-mode agents whose foci already holds an API client.
type APISummariser struct {
	defaultClient  provider.Client
	clientProvider provider.ClientProvider
	groupResolver  *config.GroupResolver
	fallbackFn     provider.FallbackFunc
	maxInputChars  func() int // rune-count cap on input; 0 disables cap
}

// NewAPISummariser builds the API-path summariser. maxInputChars is called
// fresh on each Summarise so a live config edit takes effect immediately.
func NewAPISummariser(client provider.Client, clientProvider provider.ClientProvider, groupResolver *config.GroupResolver, fallbackFn provider.FallbackFunc, maxInputChars func() int) *APISummariser {
	return &APISummariser{
		defaultClient:  client,
		clientProvider: clientProvider,
		groupResolver:  groupResolver,
		fallbackFn:     fallbackFn,
		maxInputChars:  maxInputChars,
	}
}

// resolveForCall picks the model/client/format for the summarisation call,
// preferring a config-overridden endpoint when available.
func (s *APISummariser) resolveForCall() (provider.Client, string, string) {
	resolved := s.groupResolver.ResolveCall(config.CallSummarizeFile)
	if resolved == nil {
		// Ungrouped — shouldn't happen for CallSummarizeFile, but be safe.
		return s.defaultClient, "", ""
	}
	client := s.defaultClient
	if s.clientProvider != nil {
		if c := s.clientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
			client = c
		}
	}
	return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
}

// Summarise sends the content + prompt to the configured cheap model via
// provider.Send and returns the model's text response. The call is booked in
// the cost ledger as a summary call.
func (s *APISummariser) Summarise(ctx context.Context, content []byte, prompt, filePath string) (string, error) {
	content = CapInputChars(content, s.maxInputChars())

	client, model, format := s.resolveForCall()

	req := &provider.MessageRequest{
		Model:     model,
		MaxTokens: 4096,
		System: []provider.SystemBlock{
			{Type: "text", Text: summarySystemPrompt},
		},
		Messages: []provider.Message{
			{
				Role:    "user",
				Content: provider.TextContent(summaryUserMessage(content, prompt, filePath)),
			},
		},
		SessionKey: SessionKeyFromContext(ctx),
	}

	start := time.Now()
	resp, err := provider.Send(ctx, client, req, nil,
		s.fallbackFn, s.clientProvider, func(f string, args ...any) {
			summaryLog.Errorf(f, args...)
		})
	if err != nil {
		return "", fmt.Errorf("summary API call: %w", err)
	}
	duration := time.Since(start)

	// Cost, log line and ledger row all name the tuple that served — after
	// a fallback, the hop's model (on its own format when it had its own
	// client).
	served := provider.ServedTuple(provider.ModelTuple{Model: model, Format: format}, resp)
	cost, _ := modelinfo.CostAsOf(served.Model, time.Now(), resp.Usage.Tokens())

	sessionKey := SessionKeyFromContext(ctx)
	summaryLog.Infof("session=%s model=%s input=%d output=%d cost=$%.4f duration=%s",
		sessionKey, served.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	providerFormat := served.Format
	if providerFormat == "" {
		providerFormat = "anthropic"
	}
	// A summary is one direct-API call, booked on the turn whose tool call
	// asked for it — or, asked for by no turn, on a system turn of its own.
	agentID := session.AgentIDFromKey(sessionKey)
	turn := accounting.TurnFor(ctx, accounting.OwnTurn(
		accounting.MintTurnID(sessionKey, accounting.KindSummary, start),
		sessionKey, agentID, accounting.SourceSystem, start, start.Add(duration)))
	if err := accounting.Record(turn,
		accounting.APIResponse{
			ID: resp.ID, Kind: accounting.KindSummary, Provider: providerFormat, Model: served.Model,
			Session: sessionKey, AgentID: agentID, TurnID: turn.TurnID,
			Start: start, Duration: duration, Tokens: resp.Usage.Tokens(), StopReason: resp.StopReason,
		}.Call()); err != nil {
		summaryLog.Errorf("session=%s book summary call: %v", sessionKey, err)
	}

	text := provider.TextOf(resp.Content)
	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
