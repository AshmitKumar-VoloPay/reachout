package config

import (
	"encoding/base64"
	"log/slog"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || !c.WorkerEnabled || c.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://x", "ADDR": ":9000", "WORKER_ENABLED": "false", "LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9000" || c.WorkerEnabled || c.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestLoadOriginsAndCookie(t *testing.T) {
	c, err := Load(env(map[string]string{"DATABASE_URL": "x"}))
	if err != nil || len(c.AppOrigins) != 1 || c.AppOrigins[0] != "http://localhost:5173" || c.CookieSecure {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = Load(env(map[string]string{
		"DATABASE_URL": "x", "APP_ORIGINS": "https://a.com/, https://b.com", "COOKIE_SECURE": "true",
	}))
	if err != nil || len(c.AppOrigins) != 2 || c.AppOrigins[0] != "https://a.com" || !c.CookieSecure {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	if _, err := Load(env(map[string]string{"DATABASE_URL": "x", "COOKIE_SECURE": "yes please"})); err == nil {
		t.Fatal("expected COOKIE_SECURE error")
	}
}

func TestLoadLinkedAPIAndKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	c, err := Load(env(map[string]string{
		"DATABASE_URL": "x", "LINKED_API_TOKEN": " tok ", "LINKED_API_BASE_URL": "http://localhost:9/", "TOKEN_ENCRYPTION_KEY": key,
	}))
	if err != nil || c.LinkedAPIToken != "tok" || c.LinkedAPIBaseURL != "http://localhost:9" || len(c.TokenKey) != 32 {
		t.Fatalf("%+v %v", c, err)
	}
	for name, k := range map[string]string{
		"not base64": "!!!",
		"too short":  base64.StdEncoding.EncodeToString(make([]byte, 16)),
	} {
		if _, err := Load(env(map[string]string{"DATABASE_URL": "x", "TOKEN_ENCRYPTION_KEY": k})); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if c, err := Load(env(map[string]string{"DATABASE_URL": "x", "LINKED_API_WEBHOOK_SECRET": " s3cret\n"})); err != nil || c.WebhookSecret != "s3cret" {
		t.Fatalf("webhook secret: %+v %v", c, err)
	}
	// Unset is allowed here; the server refuses to start without them.
	if c, err := Load(env(map[string]string{"DATABASE_URL": "x"})); err != nil || c.TokenKey != nil || c.LinkedAPIToken != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing dsn":   {},
		"bad worker":    {"DATABASE_URL": "x", "WORKER_ENABLED": "maybe"},
		"bad log level": {"DATABASE_URL": "x", "LOG_LEVEL": "loud"},
	}
	for name, m := range cases {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
