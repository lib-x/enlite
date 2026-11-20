package enlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/lib-x/enlite"
	"github.com/liustack/sqlite"
	"github.com/liustack/sqlite/ext/regexp"
)

func TestPoolConnections(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	dsn := filepath.Join(t.TempDir(), "app.sqlite") + "?_pragma=foreign_keys(0)&_pragma=busy_timeout(39)"
	drv, err := enlite.Open(dsn,
		enlite.WithPragma("foreign_keys(1)"),
		enlite.WithBusyTimeout(123*time.Millisecond),
		enlite.WithPragma("journal_mode(WAL)"),
		enlite.WithWALLimit(8<<20),
		enlite.WithPoolSize(2),
		enlite.WithSQLiteOptions(regexp.Option()),
		enlite.WithSQLiteOptions(sqlite.WithCollation("enlite_reverse", func(a, b string) int {
			return strings.Compare(b, a)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	if got := drv.Dialect(); got != dialect.SQLite {
		t.Fatalf("dialect = %q, want %q", got, dialect.SQLite)
	}
	db := drv.DB()
	if got := db.Stats().MaxOpenConnections; got != 2 {
		t.Fatalf("max open connections = %d, want 2", got)
	}
	if got := db.Stats().OpenConnections; got != 0 {
		t.Fatalf("open connections before first use = %d, want 0", got)
	}
	first := takeConn(t, ctx, db)
	second := takeConn(t, ctx, db)
	for _, conn := range []*sql.Conn{first, second} {
		assertInt(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys"), 1)
		assertInt(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout"), 123)
		assertInt(t, conn.QueryRowContext(ctx, "SELECT 'Ada' REGEXP '^A'"), 1)
		assertInt(t, conn.QueryRowContext(ctx, "SELECT 'z' < 'a' COLLATE enlite_reverse"), 1)
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("journal_mode = %q, err = %v, want wal", mode, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().Idle; got != 2 {
		t.Fatalf("idle connections = %d, want 2", got)
	}
}

func TestMemoryDatabases(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"", ":memory:", "file:", "file::memory:", "file:%3Amemory%3A#example",
		"file:private?mode=memory", "file:private?vfs=memdb", "file:/?vfs=memdb",
		"file://localhost/?vfs=memdb", "file:///?vfs=memdb",
		"file:private?vfs=default&vfs=memdb",
	} {
		t.Run("private "+dsn, func(t *testing.T) {
			drv, err := enlite.Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer drv.Close()
			db := drv.DB()
			if got := db.Stats().MaxOpenConnections; got != 1 {
				t.Fatalf("max open connections = %d, want 1", got)
			}
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE items (id INTEGER)"); err != nil {
				t.Fatal(err)
			}
			assertInt(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM items"), 0)
		})
	}

	for _, dsn := range []string{
		"file:/enlite-test?vfs=memdb", "file:%5Cenlite-test?vfs=memdb",
		"file://localhost/enlite-test?vfs=default&vfs=memdb",
	} {
		t.Run("shared "+dsn, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			drv, err := enlite.Open(dsn, enlite.WithPoolSize(2))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = drv.Close() })
			first := takeConn(t, ctx, drv.DB())
			second := takeConn(t, ctx, drv.DB())
			if _, err := first.ExecContext(ctx, "CREATE TABLE items (id INTEGER)"); err != nil {
				t.Fatal(err)
			}
			if _, err := first.ExecContext(ctx, "INSERT INTO items VALUES (42)"); err != nil {
				t.Fatal(err)
			}
			assertInt(t, second.QueryRowContext(ctx, "SELECT id FROM items"), 42)
			assertInt(t, second.QueryRowContext(ctx, "PRAGMA foreign_keys"), 1)
		})
	}
}

func TestImmediateTransactionLock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dsn := filepath.Join(t.TempDir(), "locking.db") + "?_txlock=deferred&_wal_limit=invalid"
	drv, err := enlite.Open(dsn,
		enlite.WithTxLock(enlite.TxImmediate),
		enlite.WithWALLimit(0),
		enlite.WithBusyTimeout(0),
		enlite.WithPoolSize(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	first := takeConn(t, ctx, drv.DB())
	second := takeConn(t, ctx, drv.DB())
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	other, err := second.BeginTx(ctx, nil)
	if err == nil {
		_ = other.Rollback()
		t.Fatal("second writer acquired the lock while the first transaction held it")
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); !ok || sqliteErr.Code() != sqlite.BUSY {
		t.Fatalf("second writer got %v, want SQLITE_BUSY", err)
	}
	read, err := second.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only transaction: %v", err)
	}
	if err := read.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	other, err = second.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("writer after rollback: %v", err)
	}
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		dsn  string
		opts []enlite.Option
		want string
	}{
		{name: "malformed query", dsn: "file:test?_pragma=%zz", want: "parse DSN parameters"},
		{name: "malformed path", dsn: "file:test%zz", want: "parse DSN path"},
		{name: "NUL in path", dsn: "file:%3Amemory%3A%00suffix", want: "NUL"},
		{name: "NUL in URI parameter", dsn: "file:private?vfs=memdb%00suffix", want: "NUL"},
		{name: "literal NUL", dsn: "test\x00suffix", want: "NUL"},
		{name: "shared cache", dsn: "file:test?cache=shared", want: "cache=shared"},
		{name: "mattn DSN option", dsn: "file:test?_fk=1", want: "unknown parameter _fk"},
		{name: "invalid raw lock", dsn: "file:test?_txlock=bad", want: "_txlock=bad"},
		{name: "invalid upstream option", opts: []enlite.Option{enlite.WithSQLiteOptions(sqlite.WithFunction("bad", 1, nil))}, want: "nil"},
		{name: "nil option", opts: []enlite.Option{nil}, want: "nil option"},
		{name: "empty PRAGMA", opts: []enlite.Option{enlite.WithPragma(" ")}, want: "empty PRAGMA"},
		{name: "negative timeout", opts: []enlite.Option{enlite.WithBusyTimeout(-time.Millisecond)}, want: "invalid busy timeout"},
		{name: "fractional timeout", opts: []enlite.Option{enlite.WithBusyTimeout(time.Microsecond)}, want: "invalid busy timeout"},
		{name: "large timeout", opts: []enlite.Option{enlite.WithBusyTimeout(1 << 62)}, want: "invalid busy timeout"},
		{name: "invalid lock", opts: []enlite.Option{enlite.WithTxLock("bad")}, want: "invalid transaction lock"},
		{name: "negative WAL limit", opts: []enlite.Option{enlite.WithWALLimit(-1)}, want: "WAL limit cannot be negative"},
		{name: "invalid pool size", opts: []enlite.Option{enlite.WithPoolSize(0)}, want: "pool size must be positive"},
		{name: "private pool size", dsn: ":memory:", opts: []enlite.Option{enlite.WithPoolSize(2)}, want: "requires a pool size of one"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			drv, err := enlite.Open(tt.dsn, tt.opts...)
			if drv != nil {
				_ = drv.Close()
				t.Fatal("Open returned a driver for invalid configuration")
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Open error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestOpenChecksDatabaseOnFirstUse(t *testing.T) {
	t.Parallel()
	drv, err := enlite.Open(filepath.Join(t.TempDir(), "missing", "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close()
	if err := drv.DB().PingContext(t.Context()); err == nil {
		t.Fatal("PingContext opened a file in a nonexistent directory")
	}
}

func takeConn(t *testing.T, ctx context.Context, db *sql.DB) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func assertInt(t *testing.T, row *sql.Row, want int) {
	t.Helper()
	var got int
	if err := row.Scan(&got); err != nil || got != want {
		t.Fatalf("query returned %d, err = %v, want %d", got, err, want)
	}
}
