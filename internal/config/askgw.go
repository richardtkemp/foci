package config

const AskgwGroupName = "foci-askgw"

type AskgwConfig struct {
	Enabled             bool     `toml:"enabled"`
	SocketPath          string   `toml:"socket_path"`
	Group               string   `toml:"group"                 default:"foci-askgw"`
	AllowedUIDs         []string `toml:"allowed_uids"`
	DefaultAgent        string   `toml:"default_agent"`
	DefaultTimeoutSecs  int      `toml:"default_timeout_seconds"`
	MaxFrameBytes       int      `toml:"max_frame_bytes"        default:"1048576"`
}

func (a AskgwConfig) ResolvedGroup() string {
	if a.Group != "" {
		return a.Group
	}
	return AskgwGroupName
}
