package enlite

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/liustack/sqlite"
)

type config struct {
	params        url.Values
	sqliteOptions []sqlite.Option
	poolSize      int
}

// Option configures the database opened by Open.
type Option func(*config) error

// WithPragma appends a SQLite PRAGMA expression, such as "journal_mode(WAL)".
// The upstream driver runs it on every physical connection. Use trusted SQL
// because the upstream driver evaluates this expression as SQL.
func WithPragma(expr string) Option {
	return func(cfg *config) error {
		if strings.TrimSpace(expr) == "" {
			return errors.New("enlite: empty PRAGMA expression")
		}
		cfg.params.Add("_pragma", expr)
		return nil
	}
}

// WithBusyTimeout sets PRAGMA busy_timeout, including the upstream writer
// queue's wait limit. The duration must be a whole number of milliseconds
// between zero and 2147483647. Zero disables waiting on locks.
func WithBusyTimeout(timeout time.Duration) Option {
	return func(cfg *config) error {
		if timeout < 0 || timeout%time.Millisecond != 0 || timeout.Milliseconds() > math.MaxInt32 {
			return fmt.Errorf("enlite: invalid busy timeout %s, want 0 to %d whole milliseconds", timeout, math.MaxInt32)
		}
		cfg.params.Add("_pragma", "busy_timeout("+strconv.FormatInt(timeout.Milliseconds(), 10)+")")
		return nil
	}
}

// TxLockMode controls how SQLite begins write transactions.
type TxLockMode string

const (
	// TxDeferred waits until the first write before acquiring a write lock.
	TxDeferred TxLockMode = "deferred"
	// TxImmediate acquires the write lock at BEGIN.
	TxImmediate TxLockMode = "immediate"
	// TxExclusive acquires the write lock at BEGIN and blocks readers outside WAL mode.
	TxExclusive TxLockMode = "exclusive"
)

// WithTxLock sets liustack/sqlite's _txlock, overriding the DSN value.
// Read-only transactions always begin deferred.
func WithTxLock(mode TxLockMode) Option {
	return func(cfg *config) error {
		switch mode {
		case TxDeferred, TxImmediate, TxExclusive:
			cfg.params.Set("_txlock", string(mode))
			return nil
		default:
			return fmt.Errorf("enlite: invalid transaction lock mode %q", mode)
		}
	}
}

// WithWALLimit sets liustack/sqlite's _wal_limit in bytes, overriding the DSN
// value. Above this threshold the driver tries to restart the WAL. Zero turns
// off this size-based restart. It takes effect only in WAL mode.
func WithWALLimit(bytes int64) Option {
	return func(cfg *config) error {
		if bytes < 0 {
			return fmt.Errorf("enlite: WAL limit cannot be negative: %d", bytes)
		}
		cfg.params.Set("_wal_limit", strconv.FormatInt(bytes, 10))
		return nil
	}
}

// WithPoolSize sets both the maximum open connections and maximum idle
// connections. Connections retain their page cache while idle. Size must be
// positive and must be one for a private in-memory or temporary database.
func WithPoolSize(size int) Option {
	return func(cfg *config) error {
		if size < 1 {
			return fmt.Errorf("enlite: pool size must be positive: %d", size)
		}
		cfg.poolSize = size
		return nil
	}
}

// WithSQLiteOptions passes upstream options to the connector for this pool.
// Use it to install SQL functions, collations, virtual tables or extensions
// such as regexp and uuid on every connection. Callbacks can run concurrently
// on different connections.
func WithSQLiteOptions(opts ...sqlite.Option) Option {
	opts = slices.Clone(opts)
	return func(cfg *config) error {
		cfg.sqliteOptions = append(cfg.sqliteOptions, opts...)
		return nil
	}
}
