// Package migrations embeds the SQL files so the server binary carries its schema.
// Files are named NNN_name.sql (up). NNN_name.down.sql files are for manual rollback
// and are never applied by the runner.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
