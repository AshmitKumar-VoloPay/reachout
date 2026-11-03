// Package config loads server settings from environment variables.
package config

import (
	"encoding/base64"
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

	// AppOrigins are the browser origins allowed to make state-changing requests (CSRF check).
	AppOrigins []string
	// CookieSecure marks the session cookie Secure. Set true behind HTTPS.
	CookieSecure bool

	// Linked API. Both are validated here when set and required by the server at startup.
	LinkedAPIToken   string
	LinkedAPIBaseURL string // empty means the public API
	TokenKey         []byte // 32 bytes, from TOKEN_ENCRYPTION_KEY (base64)
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
	origins := getenv("APP_ORIGINS")
	if origins == "" {
		origins = "http://localhost:5173" // Vite dev server
	}
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.AppOrigins = append(c.AppOrigins, o)
		}
	}
	if v := getenv("COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("COOKIE_SECURE: %w", err)
		}
		c.CookieSecure = b
	}
	c.LinkedAPIToken = strings.TrimSpace(getenv("LINKED_API_TOKEN"))
	c.LinkedAPIBaseURL = strings.TrimRight(strings.TrimSpace(getenv("LINKED_API_BASE_URL")), "/")
	if v := strings.TrimSpace(getenv("TOKEN_ENCRYPTION_KEY")); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(key) != 32 {
			return Config{}, errors.New("TOKEN_ENCRYPTION_KEY must be 32 random bytes, base64 encoded (openssl rand -base64 32)")
		}
		c.TokenKey = key
	}
	if v := strings.TrimSpace(getenv("LOG_LEVEL")); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	return c, nil
}
