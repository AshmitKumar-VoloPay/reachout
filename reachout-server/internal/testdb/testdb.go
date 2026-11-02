// Package testdb gives tests an isolated, fully migrated Postgres database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/db"
	"github.com/ashmit/reachout-server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New creates a throwaway database, applies all migrations and returns a pool.
// The database is dropped when the test ends. If no Postgres is reachable the
// test is skipped. Set TEST_DATABASE_ADMIN_URL to override the default server.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = "postgres://localhost:5432/postgres?sslmode=disable"
	}
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	defer admin.Close(ctx)

	var b [6]byte
	rand.Read(b[:])
	name := "reachout_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create test db: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a, err := pgx.Connect(c, adminDSN)
		if err != nil {
			return
		}
		defer a.Close(c)
		a.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return pool
}
