package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	TelegramToken string // TELEGRAM_BOT_TOKEN (required)
	LLMKey        string // LLM_API_KEY or ANTHROPIC_API_KEY (required)
	LLMModel      string // LLM_MODEL (default: claude-sonnet-4-5-20250514)
	DBPath        string // DB_PATH (default: /data/state.db)
	Timezone      string // TIMEZONE (default: Europe/Rome)
	LogLevel      string // LOG_LEVEL (default: info)
	MaxTokens     int    // LLM_MAX_TOKENS (default: 1024)
	PollTimeout   int    // POLL_TIMEOUT (default: 30)
	// Domain-specific config (HOTEL_NAME, etc.) belongs in the agent's own config, not here.
}

func envOrDefault(name, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func intEnvOrDefault(name string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return n, nil
}

// LoadConfig returns error if required vars are missing.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		TelegramToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		LLMKey:        strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		LLMModel:      envOrDefault("LLM_MODEL", "claude-haiku-4-5"),
		DBPath:        envOrDefault("DB_PATH", "/data/state.db"),
		Timezone:      envOrDefault("TIMEZONE", "Europe/Rome"),
		LogLevel:      envOrDefault("LOG_LEVEL", "info"),
	}
	if cfg.LLMKey == "" {
		cfg.LLMKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}

	if cfg.TelegramToken == "" {
		return nil, fmt.Errorf("missing required env var: TELEGRAM_BOT_TOKEN")
	}
	if cfg.LLMKey == "" {
		return nil, fmt.Errorf("missing required env var: LLM_API_KEY or ANTHROPIC_API_KEY")
	}

	maxTokens, err := intEnvOrDefault("LLM_MAX_TOKENS", 1024)
	if err != nil {
		return nil, err
	}
	cfg.MaxTokens = maxTokens

	pollTimeout, err := intEnvOrDefault("POLL_TIMEOUT", 30)
	if err != nil {
		return nil, err
	}
	cfg.PollTimeout = pollTimeout

	return cfg, nil
}
