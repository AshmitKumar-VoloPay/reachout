package db_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/ashmit/reachout-server/internal/db"
	"github.com/ashmit/reachout-server/internal/testdb"
	"github.com/ashmit/reachout-server/migrations"
)

func TestMigrateIsIdempotentAndOrdered(t *testing.T) {
	pool := testdb.New(t) // already migrated with the real files
	ctx := context.Background()

	again, err := db.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second run applied %v, want nothing", again)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '001_init'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("001_init not recorded once: n=%d err=%v", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (email, password_hash) VALUES ('a@b.c', 'x')`); err != nil {
		t.Fatalf("schema not usable: %v", err)
	}
}

func TestMigrateAppliesNewFilesOnlyAndSkipsDown(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"001_init.sql":       {Data: []byte(`SELECT 1;`)},
		"002_extra.sql":      {Data: []byte(`CREATE TABLE extra_t (id INT); CREATE INDEX ON extra_t (id);`)},
		"002_extra.down.sql": {Data: []byte(`DROP TABLE extra_t;`)},
	}
	applied, err := db.Migrate(ctx, pool, fsys)
	if err != nil {
		t.Fatal(err)
	}
	// 001_init was applied by testdb, so only 002_extra is new; the .down file is ignored.
	if len(applied) != 1 || applied[0] != "002_extra" {
		t.Fatalf("applied %v, want [002_extra]", applied)
	}
}

func TestMigrateRollsBackFailedFile(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"900_bad.sql": {Data: []byte(`CREATE TABLE half_done (id INT); SELECT * FROM does_not_exist;`)},
	}
	if _, err := db.Migrate(ctx, pool, fsys); err == nil {
		t.Fatal("expected error")
	}
	var exists bool
	pool.QueryRow(ctx, `SELECT to_regclass('half_done') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("failed migration left a partial table behind")
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '900_bad'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration was recorded")
	}
}
