// Package config loads server settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL   string
	Addr          string
	LogLevel      slog.Level
	WorkerEnabled bool // false stops all outbound sending
}

// Load reads settings through getenv (os.Getenv in production) so tests need no real environment.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:   getenv("DATABASE_URL"),
		Addr:          getenv("ADDR"),
		WorkerEnabled: true,
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if v := getenv("WORKER_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("WORKER_ENABLED: %w", err)
		}
		c.WorkerEnabled = b
	}
	if v := strings.TrimSpace(getenv("LOG_LEVEL")); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	return c, nil
}
