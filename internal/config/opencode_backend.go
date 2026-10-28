package config

// OpencodeBackendConfig holds defaults shared by all opencode-backed
// delegator agents. Per-agent [[agents]] backend_config values still
// apply and override these globals; scalars there win.
//
// Mirrors CCBackendConfig in structure but with opencode-specific
// fields. See OPENCODE_DELEGATOR_PLAN.md §14.1.
//
// New keys here follow the hot-reload rule at the top of
// internal/config/types.go (#2603): hot:"<when>" tag + live read + applier.
type OpencodeBackendConfig struct {
	// Binary overrides the path to the `opencode` executable.
	// Default "" → "opencode" resolved via $PATH. Integration tests
	// can point this at a stub binary.
	Binary string `toml:"binary" desc:"Path to the backend executable (default: opencode via $PATH)"`

	// Hostname is the bind address for the opencode serve subprocess.
	// Default "127.0.0.1" (loopback only — the server is never exposed
	// to the network). Per-agent backend_config overrides.
	Hostname string `toml:"hostname" default:"127.0.0.1" desc:"Bind address for the opencode serve subprocess (default 127.0.0.1)"`

	// Port is the TCP port for the opencode serve subprocess. 0 = pick
	// a free port per Server. Non-zero pins the port (useful for
	// debugging).
	Port int `toml:"port" default:"0" desc:"TCP port for the opencode serve subprocess (0 = auto-pick)"`

	// ServerAuth sets HTTP basic auth on the opencode server. Empty
	// = no auth (safe on loopback). Non-empty = requires the password
	// (passed to opencode via OPENCODE_SERVER_PASSWORD env var).
	ServerAuth string `toml:"server_auth" desc:"HTTP basic auth password for the opencode server (empty = no auth)"`

	// DefaultPermission is the opencode permission mode applied at Start
	// to each session's own ruleset (PATCH /session/:id — per session and
	// enforced). "ask" = prompt for everything; "allow" = auto-approve
	// (and no .env read guard — allow means allow); "deny" = block.
	// Like the other fields here it is folded into every opencode agent's
	// backend_config ([[agents]].backend_config.default_permission wins).
	// Deliberately NO tag default: unset must stay "" so Start sends no
	// permission request at all and opencode's own defaults stand — a
	// default:"ask" here would silently turn ask-mode on for every agent.
	DefaultPermission string `toml:"default_permission" desc:"opencode permission mode applied at Start to the session's own ruleset: ask, allow, or deny"`

	// LogLevel sets the opencode serve --log-level flag. Empty = opencode
	// default (INFO). Set to "DEBUG" for verbose projector/persistence
	// logging when diagnosing session persistence issues.
	LogLevel string `toml:"log_level" desc:"opencode serve --log-level (empty = INFO)"`

	// BatchModel is the model foci's own small background jobs run on for
	// every opencode agent, as opencode names it (provider/model). No
	// default: opencode has no model name foci could assume. Per-agent
	// backend_config.batch_model wins.
	BatchModel string `toml:"batch_model" desc:"Model for foci's own small background jobs (foci_summary, the /prompts diff summary) on opencode agents, named as opencode expects (provider/model). Empty = the agent's own model. Per-agent backend_config.batch_model wins"`
}
