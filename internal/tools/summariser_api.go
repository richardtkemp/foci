package tools

import (
	"context"
	"fmt"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/provider"
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
// provider.Send and returns the model's text response. Cost is logged to
// log.API() with call_type="summary".
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
	}

	start := time.Now()
	resp, err := provider.Send(ctx, client, req, nil,
		s.fallbackFn, s.clientProvider, func(f string, args ...any) {
			log.Errorf("summary", f, args...)
		})
	if err != nil {
		return "", fmt.Errorf("summary API call: %w", err)
	}
	duration := time.Since(start)

	cost := modelinfo.Cost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	sessionKey := SessionKeyFromContext(ctx)
	log.Infof("summary", "session=%s model=%s input=%d output=%d cost=$%.4f duration=%s",
		sessionKey, model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	providerFormat := format
	if providerFormat == "" {
		providerFormat = "anthropic"
	}
	log.API(log.APIEntry{
		Timestamp:  start,
		Provider:   providerFormat,
		Session:    sessionKey,
		Model:      model,
		Input:      resp.Usage.InputTokens,
		Output:     resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadInputTokens,
		CacheWrite: resp.Usage.CacheCreationInputTokens,
		CostUSD:    cost,
		DurationMS: duration.Milliseconds(),
		StopReason: resp.StopReason,
		CallType:   "summary",
	})

	text := provider.TextOf(resp.Content)
	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
