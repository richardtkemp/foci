package telegram

import (
	"foci/internal/config"
)

type AgentTelegramConfig interface {
	ID() string
	TelegramBotToken() string
	TelegramAllowedUsers() []string
	TelegramMultiballBots() []string
	TelegramShowToolCalls() config.ToolCallDisplay
	TelegramShowThinking() config.ShowThinking
	TelegramDisplayWidth() *int
	TelegramTableWrapLines() *int
	TelegramTableStyle() *string
	TelegramStartupNotify() *bool
	TelegramStreamOutput() *bool
	TelegramStreamInterval() string
	TelegramReceivedFilesDir() string
}

type InitParams struct {
	TelegramConfig  config.TelegramConfig
	Agents          []AgentTelegramConfig
	SecretGetter    config.SecretGetter
	StopAliases     []string
	EnableStopAlias bool
}
