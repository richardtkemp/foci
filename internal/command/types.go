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

type Request struct {
	Name      string
	Args      string
	SessionKey string
	UserID    string
}

type Response struct {
	Text    string
	DocPath string
}

type Deps struct {
	Agent        *agent.Agent
	Sessions     *session.Store
	Bootstrap    *workspace.Bootstrap
	StateStore   *state.Store
	SessionIndex *session.SessionIndex
	Config       *config.Config
	AgentConfig  config.AgentConfig

	SessionKeyFromCtx func(context.Context) string
	DefaultSessionKey func() string

	Client                provider.Client
	ResolveEndpointClient func(endpoint, format string) provider.Client
	GetClient             func(endpoint, format string) provider.Client
	PeekClient            func(endpoint, format string) provider.Client

	OnSessionEnd func(sessionKey string) error
	SendDocument func(sessionKey, path string) error

	PromptSearchDirs []string
	APILogPath       string
	ConfigPath       string
	ModelAliases     map[string]string
	ResolveModelFn   func(string) (string, string)

	BuildInfo BuildInfo
}
