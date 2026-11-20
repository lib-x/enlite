// Package enlite connects Ent's SQLite dialect to the cgo-free
// github.com/liustack/sqlite driver.
//
// # S3-compatible backups
//
// [OpenWithBackup] opens a local SQLite file and backs it up to S3-compatible
// storage. Use an S3 URI with a local parameter or supply [WithBackup] for a
// local DSN. The replica manages WAL and checkpoints and uploads commits
// asynchronously. Closing the Ent client closes the pool and then the replica.
// [BackupDriver.SyncBackup] waits for committed changes to reach the store.
// For a manual restore to a new local file, use [replica.Restore].
// See examples/s3backup for a complete program.
//
// [replica.Restore]: https://pkg.go.dev/github.com/liustack/sqlite@v0.1.7/replica#Restore
package enlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/liustack/sqlite"
)

// Open creates an Ent driver backed by liustack/sqlite. Pass it to a generated
// client with ent.NewClient(ent.Driver(drv)). Closing the client closes the
// underlying database/sql pool. Use drv.DB() to access that pool directly.
//
// Open parses the DSN and validates options. SQLite opens the first connection
// on demand. Use drv.DB().PingContext to check that the database can be opened.
// Foreign keys are enabled on each new connection by default. DSN PRAGMAs run
// after that default, and option PRAGMAs run last, in the order supplied.
//
// A private in-memory or temporary database uses one open and idle connection
// because each connection otherwise gets a separate database. For a pooled
// in-memory database, use a URI such as file:/app?vfs=memdb.
func Open(dsn string, opts ...Option) (*entsql.Driver, error) {
	if isS3DSN(dsn) {
		return nil, errors.New("enlite: use OpenWithBackup for an S3 DSN")
	}
	cfg, err := parseConfig(dsn, opts)
	if err != nil {
		return nil, err
	}
	if cfg.backup != nil || cfg.restoreOnMissing {
		return nil, errors.New("enlite: use OpenWithBackup with backup options")
	}
	connector, err := cfg.connector()
	if err != nil {
		return nil, err
	}
	return cfg.driver(connector), nil
}

func parseConfig(dsn string, opts []Option) (config, error) {
	if strings.HasPrefix(dsn, "file:") {
		dsn, _, _ = strings.Cut(dsn, "#")
	}
	name, query, _ := strings.Cut(dsn, "?")
	params, err := url.ParseQuery(query)
	if err != nil {
		return config{}, fmt.Errorf("enlite: parse DSN parameters: %w", err)
	}
	path, err := databasePath(name)
	if err != nil {
		return config{}, err
	}
	// SQLite truncates URI fields at %00. Reject it so the pool policy and
	// the engine operate on the same path and parameters.
	if strings.ContainsRune(dsn, 0) || strings.ContainsRune(path, 0) || strings.Contains(query, "%00") {
		return config{}, errors.New("enlite: DSN cannot contain NUL")
	}
	params["_pragma"] = append([]string{"foreign_keys(1)"}, params["_pragma"]...)
	cfg := config{name: name, path: path, params: params}
	for _, opt := range opts {
		if opt == nil {
			return config{}, errors.New("enlite: nil option")
		}
		if err := opt(&cfg); err != nil {
			return config{}, err
		}
	}
	if privateDatabase(path, params) {
		if cfg.poolSize > 1 {
			return config{}, errors.New("enlite: private in-memory or temporary database requires a pool size of one; use a file:/name?vfs=memdb URI to share an in-memory database")
		}
		cfg.poolSize = 1
	}
	return cfg, nil
}

func (cfg config) connector() (*sqlite.Connector, error) {
	connector, err := sqlite.NewConnector(cfg.name+"?"+cfg.params.Encode(), cfg.sqliteOptions...)
	if err != nil {
		return nil, fmt.Errorf("enlite: create SQLite connector: %w", err)
	}
	return connector, nil
}

func (cfg config) driver(connector *sqlite.Connector) *entsql.Driver {
	db := sql.OpenDB(connector)
	if cfg.poolSize > 0 {
		db.SetMaxOpenConns(cfg.poolSize)
		db.SetMaxIdleConns(cfg.poolSize)
	}
	return entsql.OpenDB(dialect.SQLite, db)
}

func databasePath(name string) (string, error) {
	path, isURI := strings.CutPrefix(name, "file:")
	if !isURI {
		return name, nil
	}
	// SQLite strips the URI authority before decoding the path. Only use the
	// decoded path to choose a pool policy and pass the URI itself upstream.
	if authority, ok := strings.CutPrefix(path, "//"); ok {
		path = ""
		if _, rest, ok := strings.Cut(authority, "/"); ok {
			path = "/" + rest
		}
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", fmt.Errorf("enlite: parse DSN path: %w", err)
	}
	return decoded, nil
}

func privateDatabase(path string, params url.Values) bool {
	if path == "" || path == ":memory:" || lastValue(params["mode"]) == "memory" {
		return true
	}
	// The memdb VFS shares only names longer than one byte that start with a slash.
	return lastValue(params["vfs"]) == "memdb" &&
		(len(path) < 2 || (path[0] != '/' && path[0] != '\\'))
}

// SQLite applies the last occurrence of URI mode and vfs parameters.
func lastValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}
