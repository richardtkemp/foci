package config

import "foci/internal/log"

var (
	configLog = log.NewComponentLogger("config")
)

const AskgwGroupName = "foci-askgw"

type AskgwConfig struct {
	Enabled            bool     `toml:"enabled" desc:"Enable the ask-gateway socket server for external apps to ask humans questions"`
	SocketPath         string   `toml:"socket_path" desc:"Unix socket path for the ask-gateway server (default: data/askgw.sock)"`
	Group              string   `toml:"group"                 default:"foci-askgw" desc:"Unix group permitted to connect to the ask-gateway socket"`
	AllowedUIDs        []string `toml:"allowed_uids" desc:"Unix user IDs permitted to connect (empty = group-based check only)"`
	DefaultAgent       string   `toml:"default_agent" desc:"Agent ID that receives ask-gateway questions not addressed to a specific agent"`
	DefaultTimeoutSecs int      `toml:"default_timeout_seconds" desc:"Default timeout for unanswered questions before auto-denial (default 0 = no timeout)"`
	MaxFrameBytes      int      `toml:"max_frame_bytes"        default:"1048576" desc:"Maximum NDJSON frame size for ask-gateway messages (1 MiB)"`
}

func (a AskgwConfig) ResolvedGroup() string {
	if a.Group != "" {
		return a.Group
	}
	return AskgwGroupName
}
