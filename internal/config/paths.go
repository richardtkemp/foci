package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"foci/internal/log"

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
		log.Warnf("config", "ResolveBotToken(%q): secret %q not found in secrets store", botName, key)
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
		log.Warnf("config", "ResolveDiscordToken(%q): secret %q not found in secrets store", botName, key)
		return ""
	}
	return v
}

// ResolvePath resolves a path. Absolute paths are returned as-is.
// Relative paths are resolved against os.UserHomeDir().
func ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warnf("config", "resolve home dir for %q: %v", p, err)
		return p
	}
	return filepath.Join(home, p)
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

// ResolveAllPaths resolves all path config fields in one place.
// Called at the end of Load(), before validate().
func (c *Config) ResolveAllPaths() {
	c.Logging.EventFile = ResolvePath(c.Logging.EventFile)
	c.Logging.APIFile = ResolvePath(c.Logging.APIFile)
	if c.Logging.PayloadFile != "" {
		c.Logging.PayloadFile = ResolvePath(c.Logging.PayloadFile)
	}
	if c.Logging.ArchiveDir != "" {
		c.Logging.ArchiveDir = ResolvePath(c.Logging.ArchiveDir)
	}
	if c.Logging.ConversationFile == "" {
		c.Logging.ConversationFile = c.DataPath("conversation.db")
	} else {
		c.Logging.ConversationFile = ResolvePath(c.Logging.ConversationFile)
	}
	if c.Sessions.Dir == "" {
		c.Sessions.Dir = c.DataPath("sessions")
	} else {
		c.Sessions.Dir = ResolvePath(c.Sessions.Dir)
	}
	if c.Sessions.BranchOrientationFacetPrompt != "" {
		c.Sessions.BranchOrientationFacetPrompt = ResolvePath(c.Sessions.BranchOrientationFacetPrompt)
	}
	if c.Sessions.BranchOrientationHeadlessPrompt != "" {
		c.Sessions.BranchOrientationHeadlessPrompt = ResolvePath(c.Sessions.BranchOrientationHeadlessPrompt)
	}
	if c.Sessions.CompactionSummaryPrompt != "" {
		c.Sessions.CompactionSummaryPrompt = ResolvePath(c.Sessions.CompactionSummaryPrompt)
	}
	// Keepalive.Prompt and Background.Prompt: path resolution handled by prompts.ResolvePrompt at runtime.
	c.WelcomeFile = ResolvePath(c.WelcomeFile)
	if c.Environment.DocsPath != "" {
		c.Environment.DocsPath = ResolvePath(c.Environment.DocsPath)
	}
	if c.Telegram.ReceivedFilesDir != "" {
		c.Telegram.ReceivedFilesDir = ResolvePath(c.Telegram.ReceivedFilesDir)
	}
	if c.Discord.ReceivedFilesDir != "" {
		c.Discord.ReceivedFilesDir = ResolvePath(c.Discord.ReceivedFilesDir)
	}
	for i := range c.Agents {
		tg := c.Agents[i].GetTelegramPlatform()
		if tg != nil && tg.ReceivedFilesDir != "" {
			tg.ReceivedFilesDir = ResolvePath(tg.ReceivedFilesDir)
		}
		dc := c.Agents[i].GetDiscordPlatform()
		if dc != nil && dc.ReceivedFilesDir != "" {
			dc.ReceivedFilesDir = ResolvePath(dc.ReceivedFilesDir)
		}
	}
}

// ParseFlags returns the config file path from command-line flags.
func ParseFlags() string {
	path := flag.String("config", "foci.toml", "path to config file")
	flag.Parse()
	return *path
}

// UnknownKeys returns the list of unrecognised key names from the TOML metadata.
// Exported for testing; Load() calls this internally and logs warnings.
func UnknownKeys(md toml.MetaData) []string {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, key := range undecoded {
		keys[i] = strings.Join(key, ".")
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
