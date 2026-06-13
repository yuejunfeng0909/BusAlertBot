package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramToken string
	LTAAccountKey string
	DataFile      string
	Timezone      string
	PollTimeout   time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		TelegramToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		LTAAccountKey: strings.TrimSpace(os.Getenv("LTA_ACCOUNT_KEY")),
		DataFile:      envOr("DATA_FILE", "data/state.db"),
		Timezone:      envOr("TIMEZONE", "Asia/Singapore"),
		PollTimeout:   50 * time.Second,
	}

	if seconds := strings.TrimSpace(os.Getenv("TELEGRAM_POLL_TIMEOUT_SECONDS")); seconds != "" {
		n, err := strconv.Atoi(seconds)
		if err != nil || n < 1 || n > 60 {
			return Config{}, fmt.Errorf("TELEGRAM_POLL_TIMEOUT_SECONDS must be between 1 and 60")
		}
		cfg.PollTimeout = time.Duration(n) * time.Second
	}
	if cfg.TelegramToken == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.LTAAccountKey == "" {
		return Config{}, fmt.Errorf("LTA_ACCOUNT_KEY is required")
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return Config{}, fmt.Errorf("invalid TIMEZONE %q: %w", cfg.Timezone, err)
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
