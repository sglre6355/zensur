package bot

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/sglre6355/zensur/internal/censor"
)

// Config is the top-level bot configuration.
//
//   - Token comes from the DISCORD_TOKEN env var.
//   - LogLevel comes from LOG_LEVEL (debug|info|warn|error, default info).
//   - Censor rules live in the YAML file pointed to by ZENSUR_CONFIG
//     (default ./config.yaml).
type Config struct {
	Token    string
	LogLevel slog.Level
	Censor   *censor.Config
}

// LoadConfig assembles a Config from environment variables and the YAML file.
func LoadConfig() (*Config, error) {
	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN env var is required")
	}

	configPath := os.Getenv("ZENSUR_CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cc, err := censor.LoadConfigFile(configPath)
	if err != nil {
		return nil, err
	}

	return &Config{
		Token:    token,
		LogLevel: parseLogLevel(os.Getenv("LOG_LEVEL")),
		Censor:   cc,
	}, nil
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
