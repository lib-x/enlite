// Package sqlite3 registers liustack/sqlite under the name Ent uses for the
// SQLite dialect. A blank import enables ent.Open("sqlite3", dsn). Include
// _pragma=foreign_keys(1) in the DSN to enable Ent schema migrations.
//
// Import only one driver that registers the name "sqlite3" in a program.
// For per-pool options and automatic foreign-key setup, use enlite.Open
// without importing this package.
package sqlite3

import (
	"database/sql"

	"entgo.io/ent/dialect"
	"github.com/liustack/sqlite"
)

func init() {
	sql.Register(dialect.SQLite, &sqlite.Driver{})
}
