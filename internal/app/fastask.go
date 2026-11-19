package app

import (
	"context"
	"fmt"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/provider"
)

// FastCallReport describes one successful fast-model call: the model that
// served it, the [endpoints] entry it went to, its token counts, duration and
// cost (CostUSD; 0 when the model has no price on the endpoint).
type FastCallReport struct {
	Model        string
	Endpoint     string
	InputTokens  int
	OutputTokens int
	Duration     time.Duration
	CostUSD      float64
}

// FastAsk is one free-form call to the fast-summary model (pin_summary_model
// on pin_summary_endpoint) for a server feature that is not an app task: the
// JIT memory query writer and judge (#3175). Model, when set, replaces
// pin_summary_model for this call; the endpoint and provider order stay the
// pin_summary_* ones.
type FastAsk struct {
	What      string // names the call in the log and the load figures
	Model     string // "" = pin_summary_model
	System    string
	User      string
	MaxTokens int
	MaxInput  int           // runes of User sent at most (0 = 8000)
	Timeout   time.Duration // 0 = 15 s
	Session   string        // the session the call is booked on
	AgentID   string
}

// fastAskDefaultMaxInput and fastAskDefaultTimeout are FastAsk's defaults.
const (
	fastAskDefaultMaxInput = 8000
	fastAskDefaultTimeout  = 15 * time.Second
)

// AskFastModel makes the call a describes and returns the model's answer as
// written (trimmed, line breaks kept). It fails when the app provider is not
// running (the fast-summary client lives in its hub), when the fast-summary
// model is off or cannot be called, or when the call fails. It never retries.
func AskFastModel(ctx context.Context, a FastAsk) (string, FastCallReport, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.pinSum == nil {
		return "", FastCallReport{}, fmt.Errorf("the fast summary model is not available (the app provider is not running)")
	}
	maxInput := a.MaxInput
	if maxInput <= 0 {
		maxInput = fastAskDefaultMaxInput
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = fastAskDefaultTimeout
	}
	var rep FastCallReport
	ask := summaryAsk{what: a.What, use: useShared, system: a.System, maxInputRunes: maxInput,
		maxTokens: a.MaxTokens, timeout: timeout, raw: true, model: a.Model, report: &rep}
	answer, err := h.pinSum.ask(ctx, ask, a.User, a.Session, a.AgentID)
	if err != nil {
		return "", FastCallReport{}, err
	}
	if answer == "" {
		return "", FastCallReport{}, fmt.Errorf("the fast summary model is off (pin_summary_model = off)")
	}
	usage := provider.Usage{InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens}
	rep.CostUSD, _ = modelinfo.CostOnEndpoint(rep.Endpoint, rep.Model, time.Now(), usage.Tokens())
	return answer, rep, nil
}
