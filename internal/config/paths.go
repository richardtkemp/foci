package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"


	"github.com/BurntSushi/toml"
)

// SecretGetter is the interface main.go uses to look up secrets.
type SecretGetter interface {
	Get(key string) (string, bool)
}

// ResolveBotToken resolves a Telegram bot token by convention.
// If botSecret is non-empty it is used as the secret key; otherwise "telegram.<botName>".
// Returns "" if botName is empty or the secret is not found.
func ResolveBotToken(botName, botSecret string, secrets SecretGetter) string {
	if botName == "" {
		return ""
	}
	key := botSecret
	if key == "" {
		key = "telegram." + botName
	}
	v, ok := secrets.Get(key)
	if !ok {
		configLog.Debugf("ResolveBotToken(%q): secret %q not found in secrets store", botName, key)
		return ""
	}
	return v
}

// ResolveDiscordToken resolves a Discord bot token by convention.
// If botSecret is non-empty it is used as the secret key; otherwise "discord.<botName>".
// Returns "" if botName is empty or the secret is not found.
func ResolveDiscordToken(botName, botSecret string, secrets SecretGetter) string {
	if botName == "" {
		return ""
	}
	key := botSecret
	if key == "" {
		key = "discord." + botName
	}
	v, ok := secrets.Get(key)
	if !ok {
		configLog.Debugf("ResolveDiscordToken(%q): secret %q not found in secrets store", botName, key)
		return ""
	}
	return v
}

var homeResolveWarnOnce sync.Once

// ResolvePath resolves a path. Absolute paths are returned as-is.
// Relative paths are resolved against os.UserHomeDir().
func ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Every relative path hits the same failure; warn once, not per-path.
		homeResolveWarnOnce.Do(func() {
			configLog.Warnf("could not resolve home dir for relative config paths (left relative): %v", err)
		})
		return p
	}
	return filepath.Join(home, p)
}

// ResolvePathPtr resolves a *string path in place if non-nil and non-empty.
func ResolvePathPtr(p *string) {
	if p != nil && *p != "" {
		*p = ResolvePath(*p)
	}
}

// DataPath resolves the path for a data file (database, state, etc.).
// If DataDir is set, the file is placed there (resolved via ResolvePath).
// Otherwise, defaults to $HOME/data/<filename>.
func (c *Config) DataPath(filename string) string {
	if c.DataDir != "" {
		return filepath.Join(ResolvePath(c.DataDir), filename)
	}
	return filepath.Join(ResolvePath("data"), filename)
}

// AgentDataPath resolves the path for a per-agent data file, stored in the
// agent's workspace under a .data subdirectory. The agent name is NOT included
// in the filename since the workspace already scopes by agent.
// Example: workspace=/home/foci/clutch → /home/foci/clutch/.data/reminders.db
func AgentDataPath(workspace, filename string) string {
	return filepath.Join(workspace, ".data", filename)
}

// avatarExts is the image extension search order for agent avatar auto-detection.
// Mirrors the image MIME set the platforms accept (png/jpeg/gif/webp), plus jpg.
var avatarExts = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// detectAvatar returns the first existing avatar image for an agent workspace,
// preferring $workspace/avatar.{ext} over $workspace/.data/avatar.{ext}, in the
// avatarExts order. Returns "" (absolute path otherwise) when none exists.
func detectAvatar(workspace string) string {
	if workspace == "" {
		return ""
	}
	for _, dir := range []string{workspace, filepath.Join(workspace, ".data")} {
		for _, ext := range avatarExts {
			p := filepath.Join(dir, "avatar"+ext)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// ResolveAllPaths resolves all path config fields in one place.
// Called at the end of Load(), before Validate().
func (c *Config) ResolveAllPaths() {
	c.Logging.EventFile = ResolvePath(c.Logging.EventFile)
	c.Logging.APIFile = ResolvePath(c.Logging.APIFile)
	c.Logging.PayloadFile = ResolvePath(c.Logging.PayloadFile)
	if c.Logging.ArchiveDir != "" {
		c.Logging.ArchiveDir = ResolvePath(c.Logging.ArchiveDir)
	}
	if filepath.IsAbs(c.Logging.APIDB) {
		// Explicit absolute path — use as-is.
	} else if c.Logging.APIDB != "" {
		c.Logging.APIDB = c.DataPath(c.Logging.APIDB)
	}
	if c.Sessions.Dir == "" {
		c.Sessions.Dir = c.DataPath("sessions")
	} else {
		c.Sessions.Dir = ResolvePath(c.Sessions.Dir)
	}
	ResolvePathPtr(c.Sessions.BranchOrientationFacetPrompt)
	ResolvePathPtr(c.Sessions.BranchOrientationHeadlessPrompt)
	ResolvePathPtr(c.Sessions.CompactionSummaryPrompt)
	// Keepalive.Prompt and Background.Prompt: path resolution handled by prompts.ResolvePrompt at runtime.
	c.WelcomeFile = ResolvePath(c.WelcomeFile)
	ResolvePathPtr(c.Environment.DocsPath)
	if c.Askgw.Enabled {
		if c.Askgw.SocketPath == "" {
			c.Askgw.SocketPath = c.DataPath("askgw.sock")
		} else {
			c.Askgw.SocketPath = ResolvePath(c.Askgw.SocketPath)
		}
		if c.Askgw.MaxFrameBytes == 0 {
			c.Askgw.MaxFrameBytes = 1 << 20
		}
	}
	for i := range c.Platforms {
		ResolvePathPtr(c.Platforms[i].Display.ReceivedFilesDir)
	}
	for i := range c.Agents {
		for j := range c.Agents[i].Platforms {
			ResolvePathPtr(c.Agents[i].Platforms[j].Display.ReceivedFilesDir)
		}
	}
}

// ParseFlags returns the config file path and the -check-config flag from the
// command line. When checkConfig is true the caller should validate the config
// and exit without starting the server (see cmd/foci-gw/checkconfig.go).
func ParseFlags() (path string, checkConfig bool) {
	p := flag.String("config", "foci.toml", "path to config file")
	check := flag.Bool("check-config", false, "validate the config file and exit (0 = will start cleanly, 1 = parse/validate error or unknown keys); does not start the server")
	flag.Parse()
	return *p, *check
}

// UnknownKeys returns the list of unrecognised key names from the TOML metadata.
// groupNames filters out [groups] string keys that were extracted separately
// (they appear undecoded because GroupsConfig uses toml:"-" for the Groups map).
// agentGroupNames does the same for each [[agents]].groups block (extractAgentGroupNames);
// the undecoded key path carries no agent index, so this checks membership across
// the union of all agents' extracted names rather than a specific agent — a false
// negative here (treating an actually-unknown "agents.groups.X" as known) is only
// possible if X happens to collide with a DIFFERENT agent's real group name, the
// same imprecision the global groupNames check already accepts.
// Exported for testing; Load() calls this internally and logs warnings.
func UnknownKeys(md toml.MetaData, groupNames map[string]string, agentGroupNames map[int]map[string]string) []string {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	var keys []string
	for _, key := range undecoded {
		path := strings.Join(key, ".")
		// Skip [groups] string keys — extracted by extractGroupNames.
		if len(key) == 2 && key[0] == "groups" && groupNames != nil {
			if _, ok := groupNames[key[1]]; ok {
				continue
			}
		}
		// Skip [[agents]].groups string keys — extracted by extractAgentGroupNames.
		if len(key) == 3 && key[0] == "agents" && key[1] == "groups" {
			known := false
			for _, names := range agentGroupNames {
				if _, ok := names[key[2]]; ok {
					known = true
					break
				}
			}
			if known {
				continue
			}
		}
		// Skip backend_config.* descendants. BackendConfig is a free-form
		// map[string]any (backend-specific settings), so nested tables under
		// it — e.g. [agents.backend_config.env] — always report as undecoded
		// even though they are intentional, not unknown keys.
		if slices.Contains(key, "backend_config") {
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

// ValidateMemoryThreshold checks that a memory threshold string is in a valid
// format: "N%" (percentage of RAM), "Nmb" (megabytes), or "Ngb" (gigabytes).
func ValidateMemoryThreshold(s string) error {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return fmt.Errorf("empty threshold")
	}
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return fmt.Errorf("invalid percentage %q: %w", s, err)
		}
		if v <= 0 || v > 100 {
			return fmt.Errorf("percentage %q must be between 0 and 100", s)
		}
		return nil
	}
	if strings.HasSuffix(s, "gb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return fmt.Errorf("invalid gigabytes %q: %w", s, err)
		}
		if v <= 0 {
			return fmt.Errorf("gigabytes %q must be positive", s)
		}
		return nil
	}
	if strings.HasSuffix(s, "mb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return fmt.Errorf("invalid megabytes %q: %w", s, err)
		}
		if v <= 0 {
			return fmt.Errorf("megabytes %q must be positive", s)
		}
		return nil
	}
	return fmt.Errorf("unknown format %q: use \"N%%\", \"Nmb\", or \"Ngb\"", s)
}

// ParseByteSize parses a human-readable byte size string like "64MB", "1GB",
// "512KB", or a plain number (bytes). Case-insensitive.
func ParseByteSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	var suffix string
	var multiplier int
	for _, pair := range []struct {
		suffix string
		mult   int
	}{
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
	} {
		if strings.HasSuffix(upper, pair.suffix) {
			suffix = pair.suffix
			multiplier = pair.mult
			break
		}
	}
	numStr := strings.TrimSpace(s[:len(s)-len(suffix)])
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", s)
	}
	if multiplier > 0 {
		return n * multiplier, nil
	}
	return n, nil
}

// ParseFileMode parses an octal file permission string like "0600" or "0640".
// Returns os.FileMode.
func ParseFileMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty file mode")
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("parse file mode %q: %w (must be octal, e.g. \"0600\")", s, err)
	}
	if n > 0777 {
		return 0, fmt.Errorf("file mode %q out of range (max 0777)", s)
	}
	return os.FileMode(n), nil
}
