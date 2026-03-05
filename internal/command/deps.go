package command

import (
	"context"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/workspace"
)

// AgentDeps bundles the per-agent references that slash commands need.
// Constructed once per agent in the main package and passed to RegisterAgentCommands.
//
// Telegram operations use callback functions to avoid importing the telegram
// package (which imports command — that would create a circular dependency).
type AgentDeps struct {
	// Core agent references
	Agent        *agent.Agent
	Sessions     *session.Store
	Bootstrap    *workspace.Bootstrap
	StateStore   *state.Store
	SessionIndex *session.SessionIndex
	Config       *config.Config
	AgentConfig  config.AgentConfig

	// Session key resolvers
	SessionKeyFromCtx func(context.Context) string
	DefaultSessionKey func() string

	// Provider client resolution
	Client                provider.Client
	ResolveEndpointClient func(endpoint, modelID string) provider.Client
	GetClient             func(endpoint, format string) provider.Client
	PeekClient            func(endpoint, format string) provider.Client

	// Callbacks (avoids importing telegram)
	OnSessionEnd func(sessionKey string) // fires session-end memory, then clears
	SendDocFn    func(path string) error // sends a document via Telegram

	// Config-derived values
	PromptSearchDirs []string
	APILogPath       string
	ConfigPath       string
	ModelAliases     map[string]string
	ResolveModelFn   func(string) (string, string)

	// Build info
	BuildInfo BuildInfo
}
